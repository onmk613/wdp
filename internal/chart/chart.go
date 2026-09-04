package chart

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"wdp/internal/model"
	"wdp/internal/playbook"
	"wdp/internal/render"
)

// Chart 是加载后的部署包。
type Chart struct {
	Meta    Meta
	Dir     string         // chart 根目录（tgz 时为解包目录）
	Values  map[string]any // 默认 values（未合并覆盖文件）
	Helpers string         // _helpers.tpl 内容
	Deploy  []*model.Play  // deploy.yaml 解析结果（必需）
	// Phases 是 deploy 之外的生命周期相位：uninstall/status/自定义（如
	// update/stop/download），按根目录 <phase>.yaml 文件名发现。
	Phases map[string][]*model.Play
	Subs   map[string]*Chart // charts/<name> → 子 chart

	schema *jsonschema.Schema // values.schema.json 编译结果（可选）
	tmpDir string             // tgz 解包临时目录（Close 时清理）
}

// IsChartPath 判断路径是否按 chart 目标处理（目录或 .tgz 包；不存在时按后缀判断）。
func IsChartPath(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return strings.HasSuffix(path, ".tgz")
	}
	return fi.IsDir() || strings.HasSuffix(path, ".tgz")
}

// Limits 是加载侧的可配上限（零值 = 内置默认）。
type Limits struct {
	MaxExtractBytes int64 // tgz 解包总量上限（0 = 内置默认 2GiB）
}

func (l Limits) extractLimit() int64 {
	if l.MaxExtractBytes > 0 {
		return l.MaxExtractBytes
	}
	return maxExtractBytes
}

// Load 加载 chart：目录或 .tgz 包。
// tgz 包会解到临时目录，使用完毕后应调用 Close（内置默认上限，见 Limits）。
func Load(path string) (*Chart, error) {
	return LoadWithLimits(path, Limits{})
}

// LoadWithLimits 同 Load，但以显式上限覆盖内置默认
// （组合根从 wdp.cfg [transfer].max_extract_mb 注入）。
func LoadWithLimits(path string, limits Limits) (*Chart, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to access chart: %w", err)
	}
	if fi.IsDir() {
		return loadDir(path)
	}
	if strings.HasSuffix(path, ".tgz") {
		return loadTgz(path, limits)
	}
	return nil, fmt.Errorf("%s is neither a chart directory nor a .tgz package", path)
}

// Open 加载 chart 并完成执行前准备：合并 values 覆盖（-f 文件与 --set 点路径）
// 并基于 helpers 构建模板引擎。返回的 chart 使用完毕后应调用 Close。
func Open(path string, valuesFiles, setArgs []string) (*Chart, map[string]any, *render.Engine, error) {
	return OpenWithLimits(path, valuesFiles, setArgs, Limits{})
}

// OpenWithLimits 同 Open，但以显式加载上限覆盖内置默认。
func OpenWithLimits(path string, valuesFiles, setArgs []string, limits Limits) (*Chart, map[string]any, *render.Engine, error) {
	ch, err := LoadWithLimits(path, limits)
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
	// 绝对化：BaseDir/playbook_dir 不随执行 cwd 漂移（相对路径在
	// delegate_to: localhost 场景曾落到只读位置）。
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	metaData, err := os.ReadFile(filepath.Join(dir, "chart.yaml"))
	if err != nil {
		return nil, fmt.Errorf("missing chart.yaml: %w", err)
	}
	var meta Meta
	if err := yaml.Unmarshal(metaData, &meta); err != nil {
		return nil, fmt.Errorf("failed to parse chart.yaml: %w", err)
	}
	if err := validateMeta(&meta); err != nil {
		return nil, err
	}

	c := &Chart{Meta: meta, Dir: dir, Subs: map[string]*Chart{}}

	if data, err := os.ReadFile(filepath.Join(dir, "values.yaml")); err == nil {
		if c.Values, err = LoadValuesYAML(data); err != nil {
			return nil, fmt.Errorf("failed to parse values.yaml: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if c.Values == nil {
		c.Values = map[string]any{}
	}

	if c.schema, err = loadSchema(dir); err != nil {
		return nil, err
	}

	if data, err := os.ReadFile(filepath.Join(dir, "_helpers.tpl")); err == nil {
		c.Helpers = string(data)
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	plays, err := playbook.Load(filepath.Join(dir, "deploy.yaml"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse deploy.yaml: %w", err)
	}
	c.Deploy = plays

	// 生命周期相位发现：根目录除保留名外的每个 <phase>.yaml 都是一个相位
	//（uninstall/status 亦循此发现，更新/停止等自定义相位同样成立）。
	// 保留名是结构文件或随包样例，不是 playbook，误当相位加载必然解析失败。
	c.Phases, err = loadPhaseFiles(dir)
	if err != nil {
		return nil, err
	}

	// charts/ 子 chart（可选，递归加载）
	if entries, err := os.ReadDir(filepath.Join(dir, "charts")); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sub, err := loadDir(filepath.Join(dir, "charts", e.Name()))
			if err != nil {
				return nil, fmt.Errorf("subchart %s: %w", e.Name(), err)
			}
			if _, dup := c.Subs[sub.Meta.Name]; dup {
				return nil, fmt.Errorf("duplicate subchart name %q in charts/ (same-named subcharts would silently override each other)", sub.Meta.Name)
			}
			c.Subs[sub.Meta.Name] = sub
		}
	}
	// 子 chart 的 deploy.yaml 仅支持单 play（hosts 等沿用父 play）
	for name, sub := range c.Subs {
		if len(sub.Deploy) > 1 {
			return nil, fmt.Errorf("subchart %s deploy.yaml contains %d plays (only one is supported)", name, len(sub.Deploy))
		}
	}
	return c, nil
}

// reservedRootYAML 是根目录不作为相位处理的 yaml 文件：结构文件与随包
// inventory 样例（inventory.yaml 是清单不是 playbook，误当相位会在解析期炸掉）。
var reservedRootYAML = map[string]bool{
	"chart.yaml":     true,
	"values.yaml":    true,
	"inventory.yaml": true,
}

// loadPhaseFiles 扫描 chart 根目录发现生命周期相位文件：<phase>.yaml（仅
// .yaml 后缀；deploy.yaml 由调用方单独加载）。文件名不符合相位名规则的
// 杂项 yaml（如带点号的 notes.extra.yaml）静默跳过，不当相位也不报错。
func loadPhaseFiles(dir string) (map[string][]*model.Play, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	phases := map[string][]*model.Play{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".yaml")
		if name == "deploy" || reservedRootYAML[e.Name()] || !phaseNameRe.MatchString(name) {
			continue
		}
		plays, err := playbook.Load(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", e.Name(), err)
		}
		phases[name] = plays
	}
	return phases, nil
}
