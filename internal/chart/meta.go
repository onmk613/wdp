package chart

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// chartNameRe 限定 chart 名字符集：name 会拼进远端 marker 路径
// （<marker_dir>/<name>/release.json）与 uninstall 的清理命令，路径
// 分隔符 / ".." 一旦混入即越界写/删——在加载入口拒绝。
var chartNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateMeta 在加载入口校验安全敏感的 chart.yaml 字段。
func validateMeta(m *Meta) error {
	if m.Name == "" {
		return errors.New("chart.yaml is missing name")
	}
	if !chartNameRe.MatchString(m.Name) || m.Name == "." || m.Name == ".." {
		return fmt.Errorf("chart.yaml name %q is invalid (allowed: letters, digits, '.', '_', '-'; path separators are rejected)", m.Name)
	}
	if m.MarkerDir != "" {
		if !filepath.IsAbs(m.MarkerDir) {
			return fmt.Errorf("chart.yaml marker_dir %q must be an absolute path", m.MarkerDir)
		}
		if clean := filepath.Clean(m.MarkerDir); clean != m.MarkerDir || clean == string(filepath.Separator) {
			return fmt.Errorf("chart.yaml marker_dir %q is invalid (must be a cleaned absolute path, not a filesystem root)", m.MarkerDir)
		}
		if strings.Contains(m.MarkerDir, "..") {
			return fmt.Errorf("chart.yaml marker_dir %q must not contain '..'", m.MarkerDir)
		}
	}
	return nil
}

// Meta 是 chart.yaml 元数据。
type Meta struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Description string   `yaml:"description"`
	Required    []string `yaml:"required"`   // 必须由调用方提供的 values 点路径（缺失即报错）
	MarkerDir   string   `yaml:"marker_dir"` // 目标机 release marker 目录（缺省 /var/lib/wdp）
	NoMarker    bool     `yaml:"no_marker"`  // 不写 release marker
	// CheckMode 声明 chart 的脚本模块（modules/<名>）支持 check 模式预演。
	// 未声明时脚本模块在 --check 下被跳过（脚本是外部代码，默认不信任其预演安全）。
	// YAML 取值：supported / true（启用）或 false（显式关闭）。
	CheckMode CheckModeSupport `yaml:"check_mode"`
	// InventoryOverride 是 values 键白名单：这些键允许被 inventory 组/主机
	// 同名变量直接覆盖（ansible 式语义的显式 opt-in）。未列入的键维持
	// "同名时 values 恒赢"，跨名覆盖仍走模板 dig 约定。
	// 仅作用于顶层 values 域（子 chart 作用域不适用）；marker/drift 的
	// values 摘要不包含覆盖结果。
	InventoryOverride []string `yaml:"inventory_override"`
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
		return fmt.Errorf("check_mode only supports supported / true / false, got %q", value.Value)
	}
	return nil
}
