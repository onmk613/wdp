package chart

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"wdp/internal/model"
	"wdp/internal/module"
	"wdp/internal/render"
)

// Package 把 chart 目录打成 <name>-<version>.tgz（包内顶层为 <name>/ 前缀）。
// 先加载校验再打包；返回产物路径。
func Package(srcDir, outDir string) (string, error) {
	c, err := Load(srcDir)
	if err != nil {
		return "", err
	}
	if outDir == "" {
		outDir = "."
	}
	out := filepath.Join(outDir, fmt.Sprintf("%s-%s.tgz", c.Meta.Name, c.Meta.Version))

	f, err := os.Create(out)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	err = filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == srcDir {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		name := c.Meta.Name + "/" + filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name: name,
			Mode: int64(info.Mode().Perm()),
			Size: info.Size(),
		}
		if d.IsDir() {
			hdr.Typeflag = tar.TypeDir
			return tw.WriteHeader(hdr)
		}
		if !info.Mode().IsRegular() {
			return nil // 跳过符号链接等非常规文件
		}
		hdr.Typeflag = tar.TypeReg
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("打包失败: %w", err)
	}
	return out, nil
}

// LintIssue 是一条校验发现。
type LintIssue struct {
	Level string // ERROR / WARN
	Path  string // 相关文件
	Msg   string
}

// Lint 静态校验 chart：结构、模块名、chart 引用、模板可渲染（用给定 values）、envs 可解析。
// 返回全部发现（可能为空）。
func Lint(c *Chart, values map[string]any) []LintIssue {
	var issues []LintIssue

	// helpers 可解析（含子 chart 合并）
	eng, err := render.NewEngine(c.CollectHelpers())
	if err != nil {
		issues = append(issues, LintIssue{ERROR, "_helpers.tpl", err.Error()})
		return issues // 引擎不可用时后续模板校验无意义
	}

	// deploy 任务树校验（含子 chart 引用、模块名与 block 组递归）
	var walk func(prefix string, ch *Chart)
	walk = func(prefix string, ch *Chart) {
		var checkTask func(label string, t *model.Task)
		checkTask = func(label string, t *model.Task) {
			if t.ChartRef != "" {
				if _, err := c.ResolveSub(t.ChartRef); err != nil {
					issues = append(issues, LintIssue{ERROR, "deploy.yaml",
						fmt.Sprintf("任务 %q 引用子 chart 失败: %v", label, err)})
				}
				return
			}
			if t.Module == "block" {
				for _, seg := range [][]*model.Task{t.Block, t.Rescue, t.Always} {
					for _, ct := range seg {
						checkTask(label+"."+ct.Label(), ct)
					}
				}
				return
			}
			if _, ok := module.Get(t.Module); !ok {
				// chart 本地脚本模块（modules/<名>）视为合法
				if module.FindScriptModule([]string{ch.Dir, c.Dir}, t.Module) == "" {
					issues = append(issues, LintIssue{ERROR, "deploy.yaml",
						fmt.Sprintf("任务 %q 使用未知模块 %q", label, t.Module)})
				}
			}
		}
		for _, play := range ch.Deploy {
			tasks := append(append([]*model.Task{}, play.Tasks...), play.Handlers...)
			for _, t := range tasks {
				checkTask(prefix+t.Label(), t)
			}
		}
		for name, sub := range ch.Subs {
			walk(name+".", sub)
		}
	}
	walk("", c)

	// 模板文件可渲染（样例域：合并 values + 占位主机名）
	sample := map[string]any{}
	for k, v := range values {
		sample[k] = v
	}
	sample["inventory_hostname"] = "lint-host"
	for _, rel := range c.TemplateFiles() {
		data, err := os.ReadFile(filepath.Join(c.Dir, rel))
		if err != nil {
			issues = append(issues, LintIssue{ERROR, rel, err.Error()})
			continue
		}
		if _, err := eng.Render(string(data), sample); err != nil {
			issues = append(issues, LintIssue{ERROR, rel, err.Error()})
		}
	}

	// envs 文件可解析
	for _, env := range c.EnvFiles() {
		data, err := os.ReadFile(filepath.Join(c.Dir, "envs", env))
		if err == nil {
			_, err = LoadValuesYAML(data)
		}
		if err != nil {
			issues = append(issues, LintIssue{ERROR, "envs/" + env, err.Error()})
		}
	}
	return issues
}

// String 渲染 LintIssue。
func (i LintIssue) String() string {
	return fmt.Sprintf("[%s] %s: %s", i.Level, i.Path, i.Msg)
}

const (
	ERROR = "ERROR"
	WARN  = "WARN"
)
