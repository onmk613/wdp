package console

// chart 文件系统助手：安全路径拼接、chart 内写文件、目录复制、打包与
// 摘要（上传与编辑器保存共用同一套实现）。

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// PackChart 把 chart 目录打包为 tgz（排序遍历，确定性输出）。
func PackChart(srcDir, dstTgz string) error {
	out, err := os.OpenFile(dstTgz, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	var paths []string
	err = filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// 只收普通文件（与 CopyDir/ReadSpecFromDir 口径对齐）：符号链接在
		// WalkDir 里 d.IsDir() 为 false，后续 os.Stat/os.Open 却会跟随它，
		// 把链接目标的内容打进制品（可经下载端点外带；Lstat 权限位还是
		// 0777）。解包侧（chart/tgz.go）已拒绝越界链接，这里独立挡一层。
		if d.Type().IsRegular() {
			paths = append(paths, path)
		}
		return nil
	})
	if err == nil {
		slices.Sort(paths)
		for _, path := range paths {
			rel, rerr := filepath.Rel(srcDir, path)
			if rerr != nil {
				err = rerr
				break
			}
			fi, serr := os.Stat(path)
			if serr != nil {
				err = serr
				break
			}
			if werr := tw.WriteHeader(&tar.Header{
				Name: filepath.ToSlash(rel), Mode: int64(fi.Mode().Perm()),
				Size: fi.Size(), ModTime: fi.ModTime(),
			}); werr != nil {
				err = werr
				break
			}
			f, oerr := os.Open(path)
			if oerr != nil {
				err = oerr
				break
			}
			_, cerr := io.Copy(tw, f)
			f.Close()
			if cerr != nil {
				err = cerr
				break
			}
		}
	}
	if cerr := tw.Close(); err == nil {
		err = cerr
	}
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dstTgz)
	}
	return err
}

// CopyDir 递归复制目录（保存修改时以底本版本为工作台）。
func CopyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		// 只复制普通文件：符号链接会被 os.Open 跟随，等于把链接目标的
		// 内容抄进新版本制品（可再经下载端点外带）；其 Lstat 权限位还是
		// 0777。解包侧已拒绝越界链接，这里独立挡一层。
		if !d.Type().IsRegular() {
			return nil
		}
		fi, ferr := d.Info()
		if ferr != nil {
			return ferr
		}
		in, oerr := os.Open(path)
		if oerr != nil {
			return oerr
		}
		defer in.Close()
		out, cerr := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode().Perm())
		if cerr != nil {
			return cerr
		}
		_, werr := io.Copy(out, in)
		clerr := out.Close()
		if werr != nil {
			return werr
		}
		return clerr
	})
}

// WriteChartFile 写入 chart 内文件（路径相对 chart 根，禁止越界）。
func WriteChartFile(root, rel, content string) error {
	abs, err := SecureJoin(root, filepath.ToSlash(rel))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, []byte(content), 0o644)
}

// SecureJoin 路径安全拼接（拒绝越出 root 的路径，含 .. 与符号链接逃逸）。
func SecureJoin(root, rel string) (string, error) {
	clean := filepath.Clean("/" + rel)
	abs := filepath.Join(root, clean)
	if !strings.HasPrefix(abs, filepath.Clean(root)+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the chart directory", rel)
	}
	return abs, nil
}

// FileSha256 计算文件 sha256（十六进制）。
func FileSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
