package chart

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// marker schema 版本：v1 只记 values 摘要（卸载等相位被迫从 values.yaml
// 默认值重新推导，实际入参没有持久化）；v2 额外记录 resolved values。
const (
	MarkerSchemaV1 = 1
	MarkerSchemaV2 = 2

	// RedactedValue 是 sensitive_values 白名单键在 marker 中的落盘占位。
	RedactedValue = "<redacted>"
)

// ValuesDigest 返回 values 的短摘要（sha256 前 12 位，marker 记录用）。
func ValuesDigest(values map[string]any) string {
	b, err := json.Marshal(values)
	if err != nil {
		return "n/a"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// Marker 是主机侧 release.json 的结构（读侧；写侧见 MarkerContent）。
// SchemaVer == MarkerSchemaV1（或 0，字段缺失）时 Values 为空——旧版
// marker 没有 resolved values，非部署相位无法从 marker 还原入参。
type Marker struct {
	Chart      string         `json:"chart"`
	Version    string         `json:"version"`
	Phase      string         `json:"phase"`
	DeployedAt string         `json:"deployed_at"`
	ValuesSHA  string         `json:"values_sha256"`
	WdpVersion string         `json:"wdp_version"`
	Values     map[string]any `json:"values,omitempty"`
	SchemaVer  int            `json:"marker_schema,omitempty"`
}

// Schema 返回 marker 的 schema 版本（缺失按 v1）。
func (m *Marker) Schema() int {
	if m.SchemaVer <= 0 {
		return MarkerSchemaV1
	}
	return m.SchemaVer
}

// ParseMarker 解析主机侧 release.json 内容。
func ParseMarker(data []byte) (*Marker, error) {
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// MarkerDir 返回 release marker 目录（meta 覆盖或默认 /var/lib/wdp）。
func (c *Chart) MarkerDir() string {
	if c.Meta.MarkerDir != "" {
		return c.Meta.MarkerDir
	}
	return "/var/lib/wdp"
}

// MarkerEnabled 报告是否写 release marker。
func (c *Chart) MarkerEnabled() bool { return !c.Meta.NoMarker }

// MarkerPath 返回指定主机的 marker 文件路径。
func (c *Chart) MarkerPath() string {
	return c.MarkerDir() + "/" + c.Meta.Name + "/release.json"
}

// ValuesDigestOf 按 marker 同一口径计算摘要：先剔除 sensitive_values
// 白名单键（脱敏值不参与摘要，真实敏感值的变化不构成 drift 信号）。
func (c *Chart) ValuesDigestOf(values map[string]any) string {
	if len(c.Meta.SensitiveValues) == 0 {
		return ValuesDigest(values)
	}
	return ValuesDigest(c.StripSensitive(values))
}

// StripSensitive 返回剔除 sensitive_values 点路径后的 values 深拷贝。
func (c *Chart) StripSensitive(values map[string]any) map[string]any {
	return removePaths(values, c.Meta.SensitiveValues)
}

// redactSensitive 返回 sensitive_values 点路径替换为 "<redacted>" 的
// values 深拷贝（marker 落盘内容；路径不存在时无操作）。
func (c *Chart) redactSensitive(values map[string]any) map[string]any {
	if len(c.Meta.SensitiveValues) == 0 {
		return values
	}
	return replacePaths(values, c.Meta.SensitiveValues, RedactedValue)
}

// RedactValues 返回把敏感点路径替换为占位符的深拷贝（始终拷贝，路径为空
// 时也是独立副本）。控制端审计记录（~/.wdp/releases）与 plan 快照同样
// 落盘 values，chart 作者声明的敏感键不应绕过 marker 口径出现在那里。
func (c *Chart) RedactValues(values map[string]any) map[string]any {
	return RedactValues(c.Meta.SensitiveValues, values)
}

// RedactValues 按敏感点路径脱敏（无 chart 上下文时的入口，如 plan 快照）。
func RedactValues(paths []string, values map[string]any) map[string]any {
	if len(paths) == 0 {
		return deepCopyValues(values)
	}
	return replacePaths(values, paths, RedactedValue)
}

// MarkerContent 构造 marker JSON 内容（phase 为产生本次 release 的相位，
// 空串按 deploy）。v2：记录 resolved values（敏感键脱敏），摘要按脱敏前
// 剔除敏感键的口径计算。
func (c *Chart) MarkerContent(wdpVersion string, values map[string]any, phase string) []byte {
	if phase == "" {
		phase = "deploy"
	}
	m := Marker{
		Chart:      c.Meta.Name,
		Version:    c.Meta.Version,
		Phase:      phase,
		DeployedAt: time.Now().UTC().Format(time.RFC3339),
		ValuesSHA:  c.ValuesDigestOf(values),
		WdpVersion: wdpVersion,
		Values:     c.redactSensitive(values),
		SchemaVer:  MarkerSchemaV2,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // "<redacted>" 落盘为字面占位而非 \u003c 转义
	enc.SetIndent("", "  ")
	_ = enc.Encode(m)
	return buf.Bytes()
}

// ErrNoMarkerValues 是 v1 marker 无法提供 resolved values 的哨兵错误
// （调用方转成"先执行一次 deploy 升级 marker"的用户提示）。
var ErrNoMarkerValues = fmt.Errorf("marker has no resolved values (written by an older wdp)")

// removePaths 返回剔除点路径列表后的深拷贝（路径不存在时无操作）。
func removePaths(values map[string]any, paths []string) map[string]any {
	out := deepCopyValues(values)
	for _, p := range paths {
		deletePath(out, p)
	}
	return out
}

// replacePaths 返回点路径列表统一替换为 repl 的深拷贝。
func replacePaths(values map[string]any, paths []string, repl any) map[string]any {
	out := deepCopyValues(values)
	for _, p := range paths {
		setLeafPath(out, p, repl)
	}
	return out
}

// deletePath 按点路径删除 values 中的叶子键（中间路径不存在时无操作）。
func deletePath(values map[string]any, path string) {
	segs, err := parsePath(path)
	if err != nil || len(segs) == 0 {
		return
	}
	cur := values
	for i, s := range segs {
		if i == len(segs)-1 {
			if !s.hasID {
				delete(cur, s.key)
			}
			return
		}
		next, ok := descend(cur, s)
		if !ok {
			return
		}
		cur = next
	}
}

// setLeafPath 按点路径写入叶子值（中间路径不存在时无操作——脱敏只改
// 已存在的键，不为不存在的路径创建子树）。
func setLeafPath(values map[string]any, path string, v any) {
	segs, err := parsePath(path)
	if err != nil || len(segs) == 0 {
		return
	}
	cur := values
	for i, s := range segs {
		if i == len(segs)-1 {
			if !s.hasID {
				if _, exists := cur[s.key]; exists {
					cur[s.key] = v
				}
			}
			return
		}
		next, ok := descend(cur, s)
		if !ok {
			return
		}
		cur = next
	}
}

// descend 沿一段路径下钻（map 键或列表下标），失败返回 false。
func descend(cur map[string]any, s pathSeg) (map[string]any, bool) {
	var v any
	if !s.hasID {
		c, ok := cur[s.key]
		if !ok {
			return nil, false
		}
		v = c
	} else {
		list, ok := cur[s.key].([]any)
		if !ok || s.idx >= len(list) {
			return nil, false
		}
		v = list[s.idx]
	}
	next, ok := v.(map[string]any)
	return next, ok
}
