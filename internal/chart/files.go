package chart

import (
	"os"
	"path/filepath"
	"strings"
)

// TemplatesDir 返回 templates 目录路径。
func (c *Chart) TemplatesDir() string { return filepath.Join(c.Dir, "templates") }

// walkTemplates 遍历 templates/ 收集模板文件相对路径，遍历错误（目录
// 不可读等）上抛。旧实现把 walk 错误吞成空列表：lint 对"templates/ 存在
// 却列不出文件"的 chart 误报全绿，渲染时才发现模板缺失。templates/
// 本身缺失不是错误（目录可选，无模板的 chart 合法）。
func (c *Chart) walkTemplates() ([]string, error) {
	var out []string
	err := filepath.WalkDir(c.TemplatesDir(), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(c.Dir, path)
		if rerr != nil {
			return rerr
		}
		out = append(out, rel)
		return nil
	})
	if err != nil && os.IsNotExist(err) {
		return nil, nil
	}
	return out, err
}

// TemplateFiles 列出 templates/ 下的模板文件相对路径。
// 校验路径（lint）用 walkTemplates 拿到错误并报 ERROR；本方法保留无错误
// 签名供既有调用方（internal/cli 渲染）沿用，不可读时返回已收集部分。
func (c *Chart) TemplateFiles() []string {
	files, _ := c.walkTemplates()
	return files
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
