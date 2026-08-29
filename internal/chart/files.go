package chart

import (
	"os"
	"path/filepath"
	"strings"
)

// TemplatesDir 返回 templates 目录路径。
func (c *Chart) TemplatesDir() string { return filepath.Join(c.Dir, "templates") }

// TemplateFiles 列出 templates/ 下的模板文件相对路径。
func (c *Chart) TemplateFiles() []string {
	var out []string
	_ = filepath.WalkDir(c.TemplatesDir(), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(c.Dir, path)
		out = append(out, rel)
		return nil
	})
	return out
}

// EnvFiles 列出 envs/ 目录下的环境文件名。
func (c *Chart) EnvFiles() []string {
	entries, err := os.ReadDir(filepath.Join(c.Dir, "envs"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && (strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml")) {
			out = append(out, e.Name())
		}
	}
	return out
}
