package chart

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"wdp/internal/model"
	"wdp/internal/playbook"
	"wdp/internal/render"
)

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

	// 生命周期 play（可选）：uninstall.yaml 逆操作 / status.yaml 只读探测
	for name, field := range map[string]*[]*model.Play{
		"uninstall.yaml": &c.Uninstall, "status.yaml": &c.Status,
	} {
		p, err := playbook.Load(filepath.Join(dir, name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("failed to parse %s: %w", name, err)
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
