package module

// 控制端归档成员选取：按名检索压缩包内的条目（basename 或完整路径），
// artifact 与 unarchive 的 members 选项共用。

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"
)

// archiveMember 是从归档中选取的一个成员（name 为归档内完整路径）。
type archiveMember struct {
	name string
	mode int64 // 权限位（0 = 归档未携带，回退 0644）
	data []byte
}

// selectArchiveMembers 在归档字节流中按名选取成员。匹配规则：条目路径与
// 成员名完全相等，或条目路径以 "/<成员名>" 结尾（basename 检索，兼容
// kubernetes/server/bin/kubelet 这类带顶层目录的发布包）；多条目命中同一
// 成员名时归档顺序在前者生效。任一成员未命中即报错（拼错文件名必须当场
// 失败，离线场景少一个二进制是灾难）。
func selectArchiveMembers(kind string, data []byte, members []string) ([]archiveMember, error) {
	for i, m := range members {
		if strings.TrimSpace(m) == "" {
			return nil, fmt.Errorf("members[%d] is empty", i)
		}
	}

	var out []archiveMember
	matched := make([]bool, len(members))
	take := func(name string, mode int64, r io.Reader) error {
		for i, m := range members {
			if matched[i] {
				continue
			}
			if !matchArchiveMemberName(name, m) {
				continue
			}
			b, err := io.ReadAll(r)
			if err != nil {
				return fmt.Errorf("failed to read archive entry %s: %w", name, err)
			}
			matched[i] = true
			out = append(out, archiveMember{name: name, mode: mode, data: b})
			return nil // 单条目只满足一个成员名
		}
		return nil
	}

	switch kind {
	case "tar", "targz":
		var r io.Reader = bytes.NewReader(data)
		if kind == "targz" {
			gz, err := gzip.NewReader(r)
			if err != nil {
				return nil, fmt.Errorf("failed to decompress archive: %w", err)
			}
			defer gz.Close()
			r = gz
		}
		tr := tar.NewReader(r)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				return finishSelect(out, matched, members)
			}
			if err != nil {
				return nil, fmt.Errorf("failed to read archive: %w", err)
			}
			if hdr.Typeflag != tar.TypeReg {
				continue
			}
			if err := take(hdr.Name, int64(hdr.FileInfo().Mode().Perm()), tr); err != nil {
				return nil, err
			}
		}
	case "zip":
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("failed to open zip archive: %w", err)
		}
		for _, f := range zr.File {
			if !f.Mode().IsRegular() {
				continue
			}
			fr, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("failed to open archive entry %s: %w", f.Name, err)
			}
			err = take(f.Name, int64(f.Mode().Perm()), fr)
			fr.Close()
			if err != nil {
				return nil, err
			}
		}
		return finishSelect(out, matched, members)
	default:
		return nil, fmt.Errorf("member selection supports .tar/.tar.gz/.tgz/.zip archives only (got %q)", kind)
	}
}

// finishSelect 校验全部成员命中，未命中的按名报错。
func finishSelect(out []archiveMember, matched []bool, members []string) ([]archiveMember, error) {
	var missing []string
	for i, ok := range matched {
		if !ok {
			missing = append(missing, members[i])
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("archive does not contain member(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// matchArchiveMemberName 判断归档条目名是否命中成员名（精确或 basename 后缀）。
func matchArchiveMemberName(entry, member string) bool {
	return entry == member || strings.HasSuffix(entry, "/"+member)
}
