package chart

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
			// 缺链接条目会导致后续模板渲染出现难以定位的文件缺失。
			//
			// 但链接目标必须校验，否则链接会指到解包根之外：控制台读
			// spec（console.ReadSpecFromDir）与保存副本（console.CopyDir）
			// 都会跟随链接，等于把服务端任意文件（含 <data>/ca/ca.key）
			// 交到上传方手里。
			//   - 绝对目标：一律让整个包加载失败（合法 chart 不需要，
			//     出现即恶意或打包器错误，静默忽略会让问题无从发现）；
			//   - 相对目标：词法归一后必须仍在解包根内，逃逸的条目跳过
			//     （保持归档可加载，只是该链接不存在）。
			if strings.HasPrefix(filepath.ToSlash(hdr.Linkname), "/") {
				os.RemoveAll(tmp)
				return nil, fmt.Errorf("chart archive entry %q has an absolute symlink target %q, refusing to extract (suspected arbitrary-file-read attempt)",
					hdr.Name, hdr.Linkname)
			}
			link, lerr := safeArchiveLink(tmp, target, hdr.Linkname)
			if lerr != nil {
				continue
			}
			_ = os.Remove(target)
			if err := os.Symlink(link, target); err != nil {
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

// safeArchiveLink 校验归档内符号链接目标并返回可安全创建的链接值。
// 调用方负责先拒绝绝对目标（见 loadTgz）。此处只处理相对目标：
// 以「链接所在目录」为基准做词法归一，结果必须仍落在解包根 root 内，
// 否则返回错误由调用方跳过该条目。
//
// 只做词法判定（不 EvalSymlinks）是刻意的：目标文件可能还没解出来。
// 归档内其它链接同样经过本函数校验，因此链式跟随不会导出根外。
func safeArchiveLink(root, linkPath, linkName string) (string, error) {
	resolved := filepath.Clean(filepath.Join(filepath.Dir(linkPath), filepath.FromSlash(linkName)))
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", fmt.Errorf("symlink target %q cannot be resolved: %w", linkName, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("symlink target %q escapes the archive root", linkName)
	}
	return filepath.FromSlash(filepath.ToSlash(linkName)), nil
}
