// Package skel 提供应用包骨架生成：内嵌最小骨架与全能力参考骨架，
// 生成物保证 lint 通过且 --check 可演练（见 skel_test.go 的 CI 门）。
// 骨架文件中的 __NAME__ 占位符在生成时替换为应用名。
package skel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"wdp/internal/i18n"
	"wdp/internal/module"
)

//go:embed all:assets
var assets embed.FS

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ValidName 校验应用名（小写字母数字与连字符，作目录/服务名安全）。
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("应用名 %q 不合法（需匹配 ^[a-z][a-z0-9-]*$）", name)
	}
	return nil
}

// Scaffold 生成应用包骨架到 dst/<name>（full=false 最小骨架，true 全能力参考）。
// 已存在的目标目录直接报错，不做任何覆盖。
func Scaffold(dst, name string, full bool) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	root := filepath.Join(dst, name)
	if _, err := os.Stat(filepath.Join(root, "chart.yaml")); err == nil {
		return "", fmt.Errorf("%s 已存在 chart.yaml，拒绝覆盖", root)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}

	variant := "assets/basic"
	if full {
		variant = "assets/full"
	}
	err := fs.WalkDir(assets, variant, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := assets.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(variant, p)
		target := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		content := strings.ReplaceAll(string(data), "__NAME__", name)
		return os.WriteFile(target, []byte(content), 0o644)
	})
	if err != nil {
		return "", fmt.Errorf("生成骨架失败: %w", err)
	}

	if full {
		// unarchive 演示制品：内容无关紧要，生成时动态打包保证结构合法
		if err := writePayload(filepath.Join(root, "files", "payload.tar.gz"), name); err != nil {
			return "", err
		}
	}
	return root, nil
}

// writePayload 生成一个最小 tar.gz（含 VERSION 与 README 两成员）。
func writePayload(path, name string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	members := []struct{ name, body string }{
		{"VERSION", name + "-0.1.0\n"},
		{"README", "wdp 应用包骨架生成的 unarchive 演示制品\n"},
	}
	for _, m := range members {
		if err := tw.WriteHeader(&tar.Header{Name: m.name, Mode: 0o644, Size: int64(len(m.body))}); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(m.body)); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// ModuleSnippet 输出指定内置模块的参数文档与示例任务（骨架模块片段）。
func ModuleSnippet(name string) (string, error) {
	m, ok := module.Get(name)
	if !ok {
		return "", fmt.Errorf("%s: %q (%s)",
			i18n.T("unknown module", "未知模块"), name, i18n.T("see the built-in module list", "查看内置模块列表"))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s — %s\n", name, m.Desc())
	params := module.Usage(m)
	if len(params) == 0 {
		fmt.Fprintf(&sb, "%s\n", i18n.T("(parameter docs pending: module does not implement UsageProvider)",
			"（参数文档待补：模块未实现 UsageProvider）"))
	} else {
		fmt.Fprintf(&sb, "%s\n", i18n.T("parameters:", "参数："))
		for _, p := range params {
			def := p.Default
			if def == "" {
				def = "-"
			}
			fmt.Fprintf(&sb, "  %-14s %-6s %s %-8s %s\n", p.Name, p.Type,
				i18n.T("default", "默认"), def, p.Desc)
		}
	}
	if ex := module.Example(m); ex != "" {
		fmt.Fprintf(&sb, "%s\n%s", i18n.T("example task:", "示例任务："), ex)
		if !strings.HasSuffix(ex, "\n") {
			sb.WriteString("\n")
		}
	}
	return sb.String(), nil
}
