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

// maxArchiveEntries 是 tgz 条目总数上限：maxExtractBytes 只按 TypeReg
// 的声明 Size 累计，海量零字节文件/空目录条目不占尺寸却同样耗尽 inode
// 与内存（解压炸弹的另一种形态）；正常 chart 包远小于此值。
const maxArchiveEntries = 20000

// loadTgz 解包到临时目录后按目录加载（防御路径穿越）。
func loadTgz(path string, limits Limits) (c *Chart, err error) {
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
	// 失败路径统一回收解包目录；成功时目录交给 Chart.Close 清理
	defer func() {
		if err != nil {
			os.RemoveAll(tmp)
		}
	}()
	if err = extractTgz(gz, tmp, limits); err != nil {
		return nil, err
	}
	root, err := locateTopDir(tmp)
	if err != nil {
		return nil, err
	}
	c, err = loadDir(root)
	if err != nil {
		return nil, err
	}
	c.tmpDir = tmp
	return c, nil
}

// extractTgz 把 tgz 流解包到 dst（防御路径穿越与解压炸弹）。
func extractTgz(gz io.Reader, dst string, limits Limits) error {
	var total int64 // 已解包累计字节（解压炸弹封顶用）
	entries := 0    // 已见条目总数（零字节条目不占尺寸，另行计数封顶）
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar: %w", err)
		}
		if entries++; entries > maxArchiveEntries {
			return fmt.Errorf("chart archive exceeds entry limit: %s is entry %d, over the %d entry total cap (suspected extraction bomb)",
				hdr.Name, entries, maxArchiveEntries)
		}
		// 解包目标经 securejoin 约束在 dst 内：.. 穿越与绝对路径条目
		// 不会越出解包根；符号链接条目不在此收敛，按目标另行校验
		//（绝对目标整体失败、逃逸相对目标跳过，见 extractSymlink）。
		target, jerr := securejoin.SecureJoin(dst, hdr.Name)
		if jerr != nil {
			return fmt.Errorf("failed to resolve unpack path %q: %w", hdr.Name, jerr)
		}
		if target == dst {
			continue // "." 等退化为解包根本身的条目跳过
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := extractSymlink(dst, target, hdr); err != nil {
				return err
			}
		case tar.TypeReg:
			if hdr.Size < 0 || total+hdr.Size > limits.extractLimit() {
				return fmt.Errorf("chart archive exceeds extract limit: %s declares %d bytes, over the %d byte total cap (suspected extraction bomb)",
					hdr.Name, hdr.Size, limits.extractLimit())
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			// Mode=0（部分打包器不填）落盘 000 权限文件会让后续读取 EACCES，
			// 回退 0644
			mode := fs.FileMode(hdr.Mode).Perm()
			if mode == 0 {
				mode = 0o644
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			// CopyN 按 hdr.Size 拷贝, 与 tar 读取器的条目边界一致;
			// 流提前截断时返回 ErrUnexpectedEOF
			if _, err := io.CopyN(out, tr, hdr.Size); err != nil {
				_ = out.Close()
				return err
			}
			total += hdr.Size
			if err := out.Close(); err != nil { // 写错误在 Close 时才暴露（截断包）
				return err
			}
		default:
			// 合法 tar 但 wdp 不支持的条目类型（hardlink/FIFO/设备节点等）
			// 显式报错而非静默丢弃：缺文件会在渲染/执行期以难定位的方式
			// 失败（"templates/x 不存在"却查不出打包环节丢了它）。pax 扩展
			// 头（TypeXHeader/TypeXGlobalHeader）由 tar.Reader 内部消化，
			// 不会走到这里。
			return fmt.Errorf("chart archive entry %q has unsupported type %s; repackage the chart containing only regular files, directories and symlinks",
				hdr.Name, tarTypeName(hdr.Typeflag))
		}
	}
	return nil
}

// extractSymlink 在解包根内重建归档符号链接条目。
// 非 wdp package 打包器的 tgz 可能含符号链接（Helm 包常见）：缺链接条目
// 会导致后续模板渲染出现难以定位的文件缺失。
//
// 但链接目标必须校验，否则链接会指到解包根之外：控制台读 spec
// （console.ReadSpecFromDir）与保存副本（console.CopyDir）都会跟随链接，
// 等于把服务端任意文件（含 <data>/ca/ca.key）交到上传方手里。
//   - 绝对目标：一律让整个包加载失败（合法 chart 不需要，出现即恶意或
//     打包器错误，静默忽略会让问题无从发现）；
//   - 相对目标：词法归一后必须仍在解包根内，逃逸的条目跳过（返回 nil，
//     保持归档可加载，只是该链接不存在）。
func extractSymlink(root, target string, hdr *tar.Header) error {
	if strings.HasPrefix(filepath.ToSlash(hdr.Linkname), "/") {
		return fmt.Errorf("chart archive entry %q has an absolute symlink target %q, refusing to extract (suspected arbitrary-file-read attempt)",
			hdr.Name, hdr.Linkname)
	}
	link, lerr := safeArchiveLink(root, target, hdr.Linkname)
	if lerr != nil {
		return nil
	}
	_ = os.Remove(target)
	if err := os.Symlink(link, target); err != nil {
		return fmt.Errorf("failed to create symlink %q: %w", hdr.Name, err)
	}
	return nil
}

// locateTopDir 定位包内顶层 chart 根（wdp package 规范为 <name>/ 前缀）：
// 唯一顶层目录时进入其内，否则解包目录本身即根。
func locateTopDir(tmp string) (string, error) {
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return "", err
	}
	root := tmp
	if len(entries) == 1 && entries[0].IsDir() {
		root = filepath.Join(tmp, entries[0].Name())
	}
	return root, nil
}

// tarTypeName 把 tar 类型旗标映射为可读名称（unsupported 条目报错用）。
func tarTypeName(f byte) string {
	switch f {
	case tar.TypeLink:
		return "hardlink"
	case tar.TypeFifo:
		return "FIFO"
	case tar.TypeChar:
		return "character device"
	case tar.TypeBlock:
		return "block device"
	case tar.TypeCont:
		return "contiguous file"
	case tar.TypeGNUSparse:
		return "GNU sparse file"
	}
	return fmt.Sprintf("type flag %q", string(rune(f)))
}

// safeArchiveLink 校验归档内符号链接目标并返回可安全创建的链接值。
// 调用方负责先拒绝绝对目标（见 extractSymlink）。此处只处理相对目标：
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
