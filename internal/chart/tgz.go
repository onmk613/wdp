package chart

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cyphar/filepath-securejoin"
)

// maxExtractBytes 是 tgz 解包总量上限（按条目声明 Size 累计）：
// 防御解压炸弹耗尽磁盘；正常 chart 包远小于此值。
const maxExtractBytes = 2 << 30 // 2 GiB

// loadTgz 解包到临时目录后按目录加载（防御路径穿越）。
func loadTgz(path string, limits Limits) (*Chart, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress: %w", err)
	}
	defer gz.Close()

	tmp, err := os.MkdirTemp("", "wdp-chart-*")
	if err != nil {
		return nil, err
	}
	var total int64 // 已解包累计字节（解压炸弹封顶用）
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			os.RemoveAll(tmp)
			return nil, fmt.Errorf("failed to read tar: %w", err)
		}
		// 解包目标经 securejoin 约束在 tmp 内: .. 穿越、绝对路径
		// 与符号链接条目均收敛为 tmp 内路径, 不会越出解包根目录.
		target, jerr := securejoin.SecureJoin(tmp, hdr.Name)
		if jerr != nil {
			os.RemoveAll(tmp)
			return nil, fmt.Errorf("failed to resolve unpack path %q: %w", hdr.Name, jerr)
		}
		if target == tmp {
			continue // "." 等退化为解包根本身的条目跳过
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				os.RemoveAll(tmp)
				return nil, err
			}
		case tar.TypeSymlink:
			// 非 wdp package 打包器的 tgz 可能含符号链接（Helm 包常见）：
			// 创建相对链接（绝对目标收敛为 tmp 内相对写法），缺链接条目
			// 会导致后续模板渲染出现难以定位的文件缺失
			rel := strings.TrimPrefix(filepath.ToSlash(hdr.Linkname), "/")
			if slices.Contains(strings.Split(rel, "/"), "..") {
				rel = ".wdp-rejected-link"
			}
			_ = os.Remove(target)
			if err := os.Symlink(filepath.FromSlash(rel), target); err != nil {
				os.RemoveAll(tmp)
				return nil, fmt.Errorf("failed to create symlink %q: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			if hdr.Size < 0 || total+hdr.Size > limits.extractLimit() {
				os.RemoveAll(tmp)
				return nil, fmt.Errorf("chart archive exceeds extract limit: %s declares %d bytes, over the %d byte total cap (suspected extraction bomb)",
					hdr.Name, hdr.Size, limits.extractLimit())
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				os.RemoveAll(tmp)
				return nil, err
			}
			// Mode=0（部分打包器不填）落盘 000 权限文件会让后续读取 EACCES，
			// 回退 0644
			mode := fs.FileMode(hdr.Mode).Perm()
			if mode == 0 {
				mode = 0o644
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				os.RemoveAll(tmp)
				return nil, err
			}
			// CopyN 按 hdr.Size 拷贝, 与 tar 读取器的条目边界一致;
			// 流提前截断时返回 ErrUnexpectedEOF
			if _, err := io.CopyN(out, tr, hdr.Size); err != nil {
				_ = out.Close()
				os.RemoveAll(tmp)
				return nil, err
			}
			total += hdr.Size
			if err := out.Close(); err != nil { // 写错误在 Close 时才暴露（截断包）
				os.RemoveAll(tmp)
				return nil, err
			}
		}
	}

	// 定位包内顶层目录（wdp package 规范为 <name>/ 前缀）
	entries, err := os.ReadDir(tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	root := tmp
	if len(entries) == 1 && entries[0].IsDir() {
		root = filepath.Join(tmp, entries[0].Name())
	}
	c, err := loadDir(root)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	c.tmpDir = tmp
	return c, nil
}
