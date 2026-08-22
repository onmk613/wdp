// 原生归档解压（agent 侧 Go 实现）：tar / tar.gz / tar.xz / zip，
// 不依赖目标机的 tar/unzip/xz 工具。控制端 unarchive 模块在 agent/push
// 通道优先走此原语，SSH 通道与旧版 agent 回退 shell 命令。
//
// 安全约束（解压不可信归档的标配，全部 fail-loud）：
//   - 条目名拒绝 ".." 成分；前导 "/" 按惯例剥除（与 GNU tar 默认一致）
//   - 符号链接/硬链接目标必须落在 dest 内；符号链接目标额外拒绝绝对路径
//     与 ".." 成分，且创建时使用归一化后的相对目标（绝对路径目标会让
//     链接指向 dest 之外，后续条目即可写穿到任意系统路径）
//   - 写文件前若目标位置已是符号链接则先删除——绝不穿过攻击者预置的
//     链接写外部路径；目录位置遇符号链接同样先删再建

package agent

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ulikunitz/xz"
	"wdp/internal/i18n"
)

// ExtractArchive 解压 src 到 dest（格式按魔数识别），返回解出的条目数。
func ExtractArchive(src, dest string) (int, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, fmt.Errorf(i18n.T("failed to read archive: %w", "读取归档失败: %w"), err)
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
			return 0, fmt.Errorf(i18n.T("gzip decompression failed: %w", "gzip 解压失败: %w"), err)
		}
		defer gz.Close()
		return extractTar(tar.NewReader(gz), dest)
	case n >= 6 && head[0] == 0xfd && head[1] == 0x37 && head[2] == 0x7a &&
		head[3] == 0x58 && head[4] == 0x5a && head[5] == 0x00:
		xr, err := xz.NewReader(f)
		if err != nil {
			return 0, fmt.Errorf(i18n.T("xz decompression failed: %w", "xz 解压失败: %w"), err)
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
		return "", errors.New(i18n.T("archive entry name is empty", "归档条目名为空"))
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".." {
			return "", fmt.Errorf(i18n.T("archive entry %q contains .., refusing to extract", "归档条目 %q 含 ..，拒绝解压"), name)
		}
	}
	return filepath.Join(dest, clean), nil
}

// checkIntermediateSymlinks 检查 dest→target 的中间路径组件（不含最终
// 落点——最终位置由 removeSymlinkAt 替换）：若存在符号链接目录，其解析
// 结果必须仍落在 dest 内，否则拒绝。预置的符号链接目录可让 MkdirAll
// 与写文件穿过链接落到 dest 之外，仅检查最终落点挡不住这种情况。
func checkIntermediateSymlinks(dest, target string) error {
	rel, err := filepath.Rel(dest, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// safeJoin 已做词法校验，这里只防御性兜底
		return fmt.Errorf(i18n.T("archive entry %q resolves outside dest", "归档条目 %q 解析到 dest 之外"), target)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	cur := dest
	for i, part := range parts {
		if part == "." || part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		if i == len(parts)-1 {
			continue // 最终落点由 removeSymlinkAt 处理（替换语义）
		}
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			resolved, rerr := filepath.EvalSymlinks(cur)
			if rerr != nil {
				return fmt.Errorf(i18n.T("symlink %s is dangling, refusing to extract through it", "符号链接 %s 悬空，拒绝穿过解压"), cur)
			}
			rd, rerr := filepath.Rel(dest, resolved)
			if rerr != nil || rd == ".." || strings.HasPrefix(rd, ".."+string(filepath.Separator)) {
				return fmt.Errorf(i18n.T("symlink directory %s points outside dest, refusing to extract", "符号链接目录 %s 指向 dest 之外，拒绝解压"), cur)
			}
		}
	}
	return nil
}

// removeSymlinkAt 若 path 当前是符号链接则删除（防写穿预置链接），返回是否删除。
func removeSymlinkAt(path string) bool {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		_ = os.Remove(path)
		return true
	}
	return false
}

// safeLinkTarget 校验归档内符号链接的目标并返回可安全使用的链接值：
// 拒绝绝对路径与含 ".." 的目标（两者都会让链接落到 dest 之外）。
// 调用方必须用返回的归一化相对目标创建链接，而非原始值——
// 仅对原始值做 safeJoin 校验是不够的，绝对路径会被剥除前导 "/" 后通过检查。
func safeLinkTarget(link string) (string, error) {
	if strings.HasPrefix(link, "/") {
		return "", fmt.Errorf(i18n.T("refusing absolute-path symlink target %q", "拒绝绝对路径符号链接目标 %q"), link)
	}
	slash := filepath.ToSlash(link)
	for _, part := range strings.Split(slash, "/") {
		if part == ".." {
			return "", fmt.Errorf(i18n.T("symlink target %q contains .., refusing to extract", "符号链接目标 %q 含 ..，拒绝解压"), link)
		}
	}
	return filepath.FromSlash(slash), nil
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
			return count, fmt.Errorf(i18n.T("failed to read archive entry: %w", "读取归档条目失败: %w"), err)
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return count, err
		}
		if err := checkIntermediateSymlinks(dest, target); err != nil {
			return count, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			removeSymlinkAt(target)
			if err := os.MkdirAll(target, hdr.FileInfo().Mode().Perm()); err != nil {
				return count, fmt.Errorf(i18n.T("failed to create directory %s: %w", "创建目录 %s 失败: %w"), hdr.Name, err)
			}
		case tar.TypeReg:
			removeSymlinkAt(target)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return count, err
			}
			if err := writeFile(target, tr, hdr.FileInfo().Mode().Perm()); err != nil {
				return count, fmt.Errorf(i18n.T("failed to write %s: %w", "写入 %s 失败: %w"), hdr.Name, err)
			}
		case tar.TypeSymlink:
			// 链接目标必须落在 dest 内：拒绝绝对路径/".."，并用归一化后的
			// 相对目标创建（不能用原始 Linkname，见 safeLinkTarget 注释）
			rel, err := safeLinkTarget(hdr.Linkname)
			if err != nil {
				return count, fmt.Errorf(i18n.T("symlink %q target is out of bounds: %w", "符号链接 %q 目标越界: %w"), hdr.Name, err)
			}
			_ = os.Remove(target) // 已存在任何类型条目均替换（tar -x 语义）
			if err := os.Symlink(rel, target); err != nil {
				return count, fmt.Errorf(i18n.T("failed to create symlink %s: %w", "创建符号链接 %s 失败: %w"), hdr.Name, err)
			}
		case tar.TypeLink:
			linkTarget, err := safeJoin(dest, hdr.Linkname)
			if err != nil {
				return count, fmt.Errorf(i18n.T("hardlink %q target is out of bounds: %w", "硬链接 %q 目标越界: %w"), hdr.Name, err)
			}
			_ = os.Remove(target)
			if err := os.Link(linkTarget, target); err != nil {
				return count, fmt.Errorf(i18n.T("failed to create hardlink %s: %w", "创建硬链接 %s 失败: %w"), hdr.Name, err)
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
		return 0, fmt.Errorf(i18n.T("failed to read zip: %w", "读取 zip 失败: %w"), err)
	}
	defer zr.Close()
	count := 0
	for _, zf := range zr.File {
		target, err := safeJoin(dest, zf.Name)
		if err != nil {
			return count, err
		}
		if err := checkIntermediateSymlinks(dest, target); err != nil {
			return count, err
		}
		mode := zf.Mode()
		switch {
		case zf.FileInfo().IsDir():
			removeSymlinkAt(target)
			if err := os.MkdirAll(target, mode.Perm()); err != nil {
				return count, fmt.Errorf(i18n.T("failed to create directory %s: %w", "创建目录 %s 失败: %w"), zf.Name, err)
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
			rel, err := safeLinkTarget(string(link))
			if err != nil {
				return count, fmt.Errorf(i18n.T("symlink %q target is out of bounds: %w", "符号链接 %q 目标越界: %w"), zf.Name, err)
			}
			_ = os.Remove(target) // 已存在任何类型条目均替换
			if err := os.Symlink(rel, target); err != nil {
				return count, fmt.Errorf(i18n.T("failed to create symlink %s: %w", "创建符号链接 %s 失败: %w"), zf.Name, err)
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
				return count, fmt.Errorf(i18n.T("failed to write %s: %w", "写入 %s 失败: %w"), zf.Name, err)
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
		_ = f.Close()
		return err
	}
	return f.Close()
}
