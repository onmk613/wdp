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
	"github.com/cyphar/filepath-securejoin"
	"gopkg.in/yaml.v3"

	"wdp/internal/model"
	"wdp/internal/playbook"
	"wdp/internal/render"
)

// Meta 是 chart.yaml 元数据。
type Meta struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Description string   `yaml:"description"`
	Required    []string `yaml:"required"`   // 必须由调用方提供的 values 点路径（缺失即报错）
	MarkerDir   string   `yaml:"marker_dir"` // 目标机 release marker 目录（缺省 /var/lib/wdp）
	NoMarker    bool     `yaml:"no_marker"`  // 不写 release marker
	// CheckMode 声明 chart 的脚本模块（modules/<名>）支持 check 模式预演。
	// 未声明时脚本模块在 --check 下被跳过（脚本为外部代码，默认不信任其预演安全）。
	// YAML 取值：supported / true（启用）或 false（显式关闭）。
	CheckMode CheckModeSupport `yaml:"check_mode"`
}

// CheckModeSupport 解析 check_mode 字段（布尔或 "supported" 字面量）。
type CheckModeSupport bool

// UnmarshalYAML 兼容 `check_mode: supported` 与 `check_mode: true`。
func (c *CheckModeSupport) UnmarshalYAML(value *yaml.Node) error {
	switch strings.ToLower(strings.TrimSpace(value.Value)) {
	case "supported", "true", "yes", "on", "1":
		*c = true
	case "", "false", "no", "off", "0":
		*c = false
	default:
		return fmt.Errorf("check_mode 仅支持 supported / true / false，得到 %q", value.Value)
	}
	return nil
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

// IsChartPath 判断路径是否按 chart 目标处理（目录或 .tgz 包；不存在时按后缀判断）。
func IsChartPath(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return strings.HasSuffix(path, ".tgz")
	}
	return fi.IsDir() || strings.HasSuffix(path, ".tgz")
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

// Open 加载 chart 并完成执行前准备：合并 values 覆盖（-f 文件与 --set 点路径）
// 并基于 helpers 构建模板引擎。返回的 chart 使用完毕后应调用 Close。
func Open(path string, valuesFiles, setArgs []string) (*Chart, map[string]any, *render.Engine, error) {
	ch, err := Load(path)
	if err != nil {
		return nil, nil, nil, err
	}
	values, err := ch.BuildValues(valuesFiles, setArgs)
	if err != nil {
		ch.Close()
		return nil, nil, nil, err
	}
	eng, err := render.NewEngine(ch.CollectHelpers())
	if err != nil {
		ch.Close()
		return nil, nil, nil, err
	}
	return ch, values, eng, nil
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

// maxExtractBytes 是 tgz 解包总量上限（按条目声明 Size 累计）：
// 防御解压炸弹耗尽磁盘；正常 chart 包远小于此值。
const maxExtractBytes = 2 << 30 // 2 GiB

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
	var total int64 // 已解包累计字节（解压炸弹封顶用）
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
		// 解包目标经 securejoin 约束在 tmp 内: .. 穿越、绝对路径
		// 与符号链接条目均收敛为 tmp 内路径, 不会越出解包根目录.
		target, jerr := securejoin.SecureJoin(tmp, hdr.Name)
		if jerr != nil {
			os.RemoveAll(tmp)
			return nil, fmt.Errorf("解包路径 %q 解析失败: %w", hdr.Name, jerr)
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
		case tar.TypeReg:
			if hdr.Size < 0 || total+hdr.Size > maxExtractBytes {
				os.RemoveAll(tmp)
				return nil, fmt.Errorf("chart 包解压超限: %s 声明 %d 字节, 超出总量上限 %d 字节（疑似解压炸弹）",
					hdr.Name, hdr.Size, maxExtractBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				os.RemoveAll(tmp)
				return nil, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				os.RemoveAll(tmp)
				return nil, err
			}
			// CopyN 按 hdr.Size 拷贝, 与 tar 读取器的条目边界一致;
			// 流提前截断时返回 ErrUnexpectedEOF
			if _, err := io.CopyN(out, tr, hdr.Size); err != nil {
				out.Close()
				os.RemoveAll(tmp)
				return nil, err
			}
			total += hdr.Size
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
