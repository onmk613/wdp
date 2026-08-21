// 原生归档解压（agent 侧 Go 实现）：tar / tar.gz / tar.xz / zip，
// 不依赖目标机的 tar/unzip/xz 工具。控制端 unarchive 模块在 agent/push
// 通道优先走此原语，SSH 通道与旧版 agent 回退 shell 命令。
//
// 安全约束（解压不可信归档的标配，全部 fail-loud）：
//   - 条目名拒绝 ".." 成分；前导 "/" 按惯例剥除（与 GNU tar 默认一致）
//   - 符号链接/硬链接目标必须落在 dest 内
//   - 写文件前若目标位置已是符号链接则先删除——绝不穿过攻击者预置的
//     链接写外部路径；目录位置遇符号链接同样先删再建

package agent

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ulikunitz/xz"
)

// ExtractArchive 解压 src 到 dest（格式按魔数识别），返回解出的条目数。
func ExtractArchive(src, dest string) (int, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, fmt.Errorf("读取归档失败: %w", err)
	}
	defer f.Close()
	head := make([]byte, 6)
	n, _ := f.ReadAt(head, 0)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}

	switch {
	case n >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		gz, err := gzip.NewReader(f)
		if err != nil {
			return 0, fmt.Errorf("gzip 解压失败: %w", err)
		}
		defer gz.Close()
		return extractTar(tar.NewReader(gz), dest)
	case n >= 6 && head[0] == 0xfd && head[1] == 0x37 && head[2] == 0x7a &&
		head[3] == 0x58 && head[4] == 0x5a && head[5] == 0x00:
		xr, err := xz.NewReader(f)
		if err != nil {
			return 0, fmt.Errorf("xz 解压失败: %w", err)
		}
		return extractTar(tar.NewReader(xr), dest)
	case n >= 4 && string(head[:4]) == "PK\x03\x04":
		return extractZip(src, dest)
	default:
		return extractTar(tar.NewReader(f), dest)
	}
}

// safeJoin 计算归档条目在 dest 内的落盘路径：剥除前导 "/"，拒绝 ".."。
func safeJoin(dest, name string) (string, error) {
	name = strings.TrimPrefix(filepath.ToSlash(name), "/")
	clean := filepath.FromSlash(name)
	if clean == "" || clean == "." {
		return "", fmt.Errorf("归档条目名为空")
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".." {
			return "", fmt.Errorf("归档条目 %q 含 ..，拒绝解压", name)
		}
	}
	return filepath.Join(dest, clean), nil
}

// removeSymlinkAt 若 path 当前是符号链接则删除（防写穿预置链接），返回是否删除。
func removeSymlinkAt(path string) bool {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		_ = os.Remove(path)
		return true
	}
	return false
}

// extractTar 逐条目解压 tar 流。
func extractTar(tr *tar.Reader, dest string) (int, error) {
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return count, fmt.Errorf("读取归档条目失败: %w", err)
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return count, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			removeSymlinkAt(target)
			if err := os.MkdirAll(target, hdr.FileInfo().Mode().Perm()); err != nil {
				return count, fmt.Errorf("创建目录 %s 失败: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			removeSymlinkAt(target)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return count, err
			}
			if err := writeFile(target, tr, hdr.FileInfo().Mode().Perm()); err != nil {
				return count, fmt.Errorf("写入 %s 失败: %w", hdr.Name, err)
			}
		case tar.TypeSymlink:
			// 链接目标也必须落在 dest 内（相对 dest 计算）
			if _, err := safeJoin(dest, hdr.Linkname); err != nil {
				return count, fmt.Errorf("符号链接 %q 目标越界: %w", hdr.Name, err)
			}
			_ = os.Remove(target) // 已存在任何类型条目均替换（tar -x 语义）
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return count, fmt.Errorf("创建符号链接 %s 失败: %w", hdr.Name, err)
			}
		case tar.TypeLink:
			linkTarget, err := safeJoin(dest, hdr.Linkname)
			if err != nil {
				return count, fmt.Errorf("硬链接 %q 目标越界: %w", hdr.Name, err)
			}
			_ = os.Remove(target)
			if err := os.Link(linkTarget, target); err != nil {
				return count, fmt.Errorf("创建硬链接 %s 失败: %w", hdr.Name, err)
			}
		default:
			// 其余类型（fifo/device 等）静默跳过——agent 不引入设备文件
			continue
		}
		count++
	}
}

// extractZip 解压 zip 归档（随机访问，走文件名重开）。
func extractZip(src, dest string) (int, error) {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return 0, fmt.Errorf("读取 zip 失败: %w", err)
	}
	defer zr.Close()
	count := 0
	for _, zf := range zr.File {
		target, err := safeJoin(dest, zf.Name)
		if err != nil {
			return count, err
		}
		mode := zf.Mode()
		switch {
		case zf.FileInfo().IsDir():
			removeSymlinkAt(target)
			if err := os.MkdirAll(target, mode.Perm()); err != nil {
				return count, fmt.Errorf("创建目录 %s 失败: %w", zf.Name, err)
			}
		case mode&fs.ModeSymlink != 0:
			rc, err := zf.Open()
			if err != nil {
				return count, err
			}
			link, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return count, err
			}
			if _, err := safeJoin(dest, string(link)); err != nil {
				return count, fmt.Errorf("符号链接 %q 目标越界: %w", zf.Name, err)
			}
			_ = os.Remove(target) // 已存在任何类型条目均替换
			if err := os.Symlink(string(link), target); err != nil {
				return count, fmt.Errorf("创建符号链接 %s 失败: %w", zf.Name, err)
			}
		default:
			rc, err := zf.Open()
			if err != nil {
				return count, err
			}
			removeSymlinkAt(target)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				rc.Close()
				return count, err
			}
			err = writeFile(target, rc, mode.Perm())
			rc.Close()
			if err != nil {
				return count, fmt.Errorf("写入 %s 失败: %w", zf.Name, err)
			}
		}
		count++
	}
	return count, nil
}

// writeFile 写入单个文件（截断覆盖，语义同 tar -x）。
func writeFile(path string, r io.Reader, mode fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
