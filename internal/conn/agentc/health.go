package agentc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// HealthInfo 是 /health 的响应（agentctl 巡检用；零值字段按缺省理解）。
type HealthInfo struct {
	OK             bool   `json:"ok"`
	Version        string `json:"version"`
	Hostname       string `json:"hostname"`
	Goos           string `json:"goos"`
	Arch           string `json:"arch"`
	Pid            int    `json:"pid"`
	CertNotAfter   string `json:"cert_not_after"`   // RFC3339（未启用 mTLS 为空）
	IdleTimeoutSec int64  `json:"idle_timeout_sec"` // 0 = 永不
	IdleLeftSec    int64  `json:"idle_left_sec"`    // -1 = 永不
}

// Health 拉取并解析 /health（证书到期巡检、空闲剩余时间）。
func (c *Conn) Health(ctx context.Context) (*HealthInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent health HTTP %d", resp.StatusCode)
	}
	var out HealthInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("failed to parse agent response: %w", err)
	}
	return &out, nil
}
