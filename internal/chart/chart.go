package chart

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"

	"wdp/internal/model"
	"wdp/internal/playbook"
)

// Meta 是 chart.yaml 元数据。
type Meta struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Description string   `yaml:"description"`
	Required    []string `yaml:"required"`   // 必须由调用方提供的 values 点路径（缺失即报错）
	MarkerDir   string   `yaml:"marker_dir"` // 目标机 release marker 目录（缺省 /var/lib/wdp）
	NoMarker    bool     `yaml:"no_marker"`  // 不写 release marker
}

// Chart 是加载后的部署包。
type Chart struct {
	Meta      Meta
	Dir       string            // chart 根目录（tgz 时为解包目录）
	Values    map[string]any    // 默认 values（未合并覆盖文件）
	Helpers   string            // _helpers.tpl 内容
	Deploy    []*model.Play     // deploy.yaml 解析结果
	Uninstall []*model.Play     // uninstall.yaml（可选）：逆操作清单
	Status    []*model.Play     // status.yaml（可选）：只读探测
	Subs      map[string]*Chart // charts/<name> → 子 chart

	tmpDir string // tgz 解包临时目录（Close 时清理）
}

// Load 加载 chart：目录或 .tgz 包。
// tgz 包会解到临时目录，使用完毕后应调用 Close。
func Load(path string) (*Chart, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("访问 chart 失败: %w", err)
	}
	if fi.IsDir() {
		return loadDir(path)
	}
	if strings.HasSuffix(path, ".tgz") {
		return loadTgz(path)
	}
	return nil, fmt.Errorf("%s 既不是 chart 目录也不是 .tgz 包", path)
}

// Close 释放资源（tgz 临时目录）。
func (c *Chart) Close() error {
	if c.tmpDir != "" {
		return os.RemoveAll(c.tmpDir)
	}
	return nil
}

func loadDir(dir string) (*Chart, error) {
	metaData, err := os.ReadFile(filepath.Join(dir, "chart.yaml"))
	if err != nil {
		return nil, fmt.Errorf("缺少 chart.yaml: %w", err)
	}
	var meta Meta
	if err := yaml.Unmarshal(metaData, &meta); err != nil {
		return nil, fmt.Errorf("解析 chart.yaml 失败: %w", err)
	}
	if meta.Name == "" {
		return nil, fmt.Errorf("chart.yaml 缺少 name")
	}

	c := &Chart{Meta: meta, Dir: dir, Subs: map[string]*Chart{}}

	if data, err := os.ReadFile(filepath.Join(dir, "values.yaml")); err == nil {
		if c.Values, err = LoadValuesYAML(data); err != nil {
			return nil, fmt.Errorf("解析 values.yaml 失败: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if c.Values == nil {
		c.Values = map[string]any{}
	}

	if data, err := os.ReadFile(filepath.Join(dir, "_helpers.tpl")); err == nil {
		c.Helpers = string(data)
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	plays, err := playbook.Load(filepath.Join(dir, "deploy.yaml"))
	if err != nil {
		return nil, fmt.Errorf("解析 deploy.yaml 失败: %w", err)
	}
	c.Deploy = plays

	// 生命周期 play（可选）：uninstall.yaml 逆操作 / status.yaml 只读探测
	for name, field := range map[string]*[]*model.Play{
		"uninstall.yaml": &c.Uninstall, "status.yaml": &c.Status,
	} {
		p, err := playbook.Load(filepath.Join(dir, name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("解析 %s 失败: %w", name, err)
		}
		*field = p
	}

	// charts/ 子 chart（可选，递归加载）
	if entries, err := os.ReadDir(filepath.Join(dir, "charts")); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sub, err := loadDir(filepath.Join(dir, "charts", e.Name()))
			if err != nil {
				return nil, fmt.Errorf("子 chart %s: %w", e.Name(), err)
			}
			c.Subs[sub.Meta.Name] = sub
		}
	}
	// 子 chart 的 deploy.yaml 仅支持单 play（hosts 等沿用父 play）
	for name, sub := range c.Subs {
		if len(sub.Deploy) > 1 {
			return nil, fmt.Errorf("子 chart %s 的 deploy.yaml 含 %d 个 play（仅支持单个）", name, len(sub.Deploy))
		}
	}
	return c, nil
}

// loadTgz 解包到临时目录后按目录加载（防御路径穿越）。
func loadTgz(path string) (*Chart, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("解压失败: %w", err)
	}
	defer gz.Close()

	tmp, err := os.MkdirTemp("", "wdp-chart-*")
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			os.RemoveAll(tmp)
			return nil, fmt.Errorf("读取 tar 失败: %w", err)
		}
		name := filepath.Clean(hdr.Name)
		if name == "." || strings.HasPrefix(name, "..") {
			continue
		}
		target := filepath.Join(tmp, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				os.RemoveAll(tmp)
				return nil, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				os.RemoveAll(tmp)
				return nil, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				os.RemoveAll(tmp)
				return nil, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				os.RemoveAll(tmp)
				return nil, err
			}
			out.Close()
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

// FindSub 按名递归查找子 chart（支持嵌套引用）。
func (c *Chart) FindSub(name string) *Chart {
	if sub, ok := c.Subs[name]; ok {
		return sub
	}
	for _, sub := range c.Subs {
		if found := sub.FindSub(name); found != nil {
			return found
		}
	}
	return nil
}

// ResolveSub 解析子 chart 引用：`jdk` 或 `jdk@1.2.0`（版本约束，semver 语法，
// 如 jdk@^1.2 / jdk@>=1.0,<2.0）。执行、lint、template 预览统一走本入口，
// 保证版本语义不分叉。
func (c *Chart) ResolveSub(ref string) (*Chart, error) {
	name, constraint, constrained := strings.Cut(ref, "@")
	sub := c.FindSub(name)
	if sub == nil {
		return nil, fmt.Errorf("子 chart %q 不存在", name)
	}
	if constrained && constraint != "" {
		v, err := semver.NewVersion(sub.Meta.Version)
		if err != nil {
			return nil, fmt.Errorf("子 chart %s 的 version %q 不是语义化版本，无法应用约束 %q",
				name, sub.Meta.Version, constraint)
		}
		rng, err := semver.NewConstraint(constraint)
		if err != nil {
			return nil, fmt.Errorf("解析版本约束 %q 失败: %w", constraint, err)
		}
		if !rng.Check(v) {
			return nil, fmt.Errorf("子 chart %s 版本 %s 不满足约束 %q", name, sub.Meta.Version, constraint)
		}
	}
	return sub, nil
}

// CollectHelpers 汇集自身与全部子 chart 的 _helpers.tpl（父在前，子重名覆盖）。
func (c *Chart) CollectHelpers() string {
	parts := []string{}
	if c.Helpers != "" {
		parts = append(parts, c.Helpers)
	}
	var walk func(sub *Chart)
	walk = func(sub *Chart) {
		if sub.Helpers != "" {
			parts = append(parts, sub.Helpers)
		}
		for _, s := range sub.Subs {
			walk(s)
		}
	}
	for _, sub := range c.Subs {
		walk(sub)
	}
	return strings.Join(parts, "\n")
}

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

// BuildValues 构建最终 values：默认 values → 依序合并 -f 文件 → --set 参数。
func (c *Chart) BuildValues(files []string, sets []string) (map[string]any, error) {
	merged := map[string]any{}
	for k, v := range c.Values {
		merged[k] = v
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("读取 values 文件失败: %w", err)
		}
		ov, err := LoadValuesYAML(data)
		if err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", f, err)
		}
		merged = Merge(merged, ov)
	}
	for _, s := range sets {
		var err error
		if merged, err = ApplySet(merged, s); err != nil {
			return nil, err
		}
	}
	return merged, nil
}
