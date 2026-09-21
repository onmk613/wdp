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
	if err := packageTo(f, c, srcDir); err != nil {
		// 失败清理半成品（忽略"不存在"）：留下损坏 tgz 会被后续加载误用
		f.Close()
		os.Remove(out)
		return "", err
	}
	// f 的 close 与 tar/gzip 同属收尾链：此处失败同样意味着产物损坏
	if err := f.Close(); err != nil {
		os.Remove(out)
		return "", fmt.Errorf("failed to finalize package: %w", err)
	}
	return out, nil
}

// packageTo 把 chart 目录打包写入 w（包内顶层 <name>/ 前缀）。
// 从 Package 提取出来：接受任意 io.Writer，使写失败/close 失败路径可用
// 自定义 Writer 直接驱动测试（磁盘满难以在测试里模拟）。
// 约束：err 必须是命名返回值——close 链（tar/gzip flush）的错误只能在 defer
// 里上抛，未命名返回值的 `return nil` 已固定结果，defer 再赋值也改不动。
func packageTo(w io.Writer, c *Chart, srcDir string) (err error) {
	// 写路径 close 链：tar/gzip flush 的错误必须上抛（defer 会吞掉截断包）。
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	defer func() {
		closeErr := tw.Close()
		if closeErr == nil {
			closeErr = gz.Close()
		}
		if closeErr == nil {
			return
		}
		if err == nil {
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
		// 符号链接必须落包：加载侧（tgz.go）支持 TypeSymlink 条目，静默
		// 跳过会造成"目录能跑、打包后缺文件"。Size 恒 0，目标在 Linkname。
		if info.Mode()&os.ModeSymlink != 0 {
			target, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = target
			hdr.Size = 0
			return tw.WriteHeader(hdr)
		}
		if !info.Mode().IsRegular() {
			return nil // 跳过 socket 等其它非常规文件
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
		return fmt.Errorf("failed to package: %w", err)
	}
	return nil
}
