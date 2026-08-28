package chart

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Package 把 chart 目录打成 <name>-<version>.tgz（包内顶层为 <name>/ 前缀）。
// 先加载校验再打包；返回产物路径。
func Package(srcDir, outDir string) (string, error) {
	c, err := Load(srcDir)
	if err != nil {
		return "", err
	}
	if outDir == "" {
		outDir = "."
	}
	out := filepath.Join(outDir, fmt.Sprintf("%s-%s.tgz", c.Meta.Name, c.Meta.Version))

	f, err := os.Create(out)
	if err != nil {
		return "", err
	}
	// 写路径 close 链：tar/gzip flush 的错误必须上抛（defer 会吞掉截断包）
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	defer func() {
		closeErr := tw.Close()
		if closeErr == nil {
			closeErr = gz.Close()
		}
		if closeErr == nil {
			closeErr = f.Close()
		}
		if err == nil && closeErr != nil {
			err = fmt.Errorf("failed to finalize package: %w", closeErr)
		}
	}()

	err = filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == srcDir {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		name := c.Meta.Name + "/" + filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name: name,
			Mode: int64(info.Mode().Perm()),
			Size: info.Size(),
		}
		if d.IsDir() {
			hdr.Typeflag = tar.TypeDir
			return tw.WriteHeader(hdr)
		}
		if !info.Mode().IsRegular() {
			return nil // 跳过符号链接等非常规文件
		}
		hdr.Typeflag = tar.TypeReg
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		// 立即关闭：defer 会把句柄挂到整个 WalkDir 结束，大 chart 打包时 fd 线性累积
		_, copyErr := io.Copy(tw, src)
		closeErr := src.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return "", fmt.Errorf("failed to package: %w", err)
	}
	return out, nil
}
