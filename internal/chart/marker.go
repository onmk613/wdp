package chart

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
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

// MarkerContent 构造 marker JSON 内容。
func (c *Chart) MarkerContent(wdpVersion string, values map[string]any) []byte {
	type marker struct {
		Chart      string `json:"chart"`
		Version    string `json:"version"`
		Phase      string `json:"phase"`
		DeployedAt string `json:"deployed_at"`
		ValuesSHA  string `json:"values_sha256"`
		WdpVersion string `json:"wdp_version"`
	}
	b, _ := json.MarshalIndent(marker{
		Chart:      c.Meta.Name,
		Version:    c.Meta.Version,
		Phase:      "deploy",
		DeployedAt: time.Now().UTC().Format(time.RFC3339),
		ValuesSHA:  ValuesDigest(values),
		WdpVersion: wdpVersion,
	}, "", "  ")
	return b
}
