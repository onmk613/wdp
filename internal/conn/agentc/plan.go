package agentc

// 自治执行的客户端（docs/15 §7.1）：提交 plan（异步，gzip 压缩）、轮询
// 进度（journal 增量）、请求中止。结构体与 agent 端解耦（HTTP 协议形状），
// 控制端与 agent 版本可独立演进。

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"wdp/internal/plan"
)

// PlanSubmit 是 POST /plan 请求。
type PlanSubmit struct {
	RunID          string
	Plan           *plan.Plan
	LocalHost      string // 计划中属于 agent 本机的主机名（空 = 纯中继）
	Resume         bool
	BecomePassword string
	Forks          int
}

// PlanSubmitResp 是 POST /plan 响应。
type PlanSubmitResp struct {
	RunID          string         `json:"run_id"`
	Accepted       bool           `json:"accepted"`
	State          string         `json:"state"`
	ResumedFromIdx map[string]int `json:"resumed_from_idx"`
}

// PlanJournalEntry 与 agent 端 journal 行一致（协议形状）。
type PlanJournalEntry struct {
	Seq     int64     `json:"seq"`
	Host    string    `json:"host"`
	Idx     int       `json:"idx"`
	Label   string    `json:"label"`
	State   string    `json:"state"`
	Changed bool      `json:"changed"`
	Msg     string    `json:"msg,omitempty"`
	At      time.Time `json:"at"`
}

// PlanStatus 是 GET /plan/status 响应。
type PlanStatus struct {
	RunID   string             `json:"run_id"`
	PlanID  string             `json:"plan_id"`
	State   string             `json:"state"`
	Journal []PlanJournalEntry `json:"journal"`
	Error   string             `json:"error,omitempty"`
}

// ErrPlanUnsupported 表示 agent 无 /plan 端点（旧版 agent）——控制端收到
// 后回退逐任务远程执行路径（与 handleArchive 的 404 回退同惯例）。
var ErrPlanUnsupported = fmt.Errorf("agent has no /plan endpoint (older agent)")

// SubmitPlan 提交 plan（gzip：plan 是 JSON，压缩后通常缩一个数量级）。
// 返回 ErrPlanUnsupported 时调用方应回退远程执行路径。
func (c *Conn) SubmitPlan(ctx context.Context, sub *PlanSubmit) (*PlanSubmitResp, error) {
	body, err := json.Marshal(map[string]any{
		"run_id":          sub.RunID,
		"plan":            sub.Plan,
		"local_host":      sub.LocalHost,
		"resume":          sub.Resume,
		"become_password": sub.BecomePassword,
		"forks":           sub.Forks,
	})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(body); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/plan", &buf)
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Content-Encoding", "gzip")
	resp, err := c.client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrPlanUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("agent plan submit HTTP %d: %s", resp.StatusCode, string(msg))
	}
	var out PlanSubmitResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("failed to parse agent response: %w", err)
	}
	return &out, nil
}

// PlanStatus 拉取进度增量（sinceSeq 游标，返回值含新游标所在条目）。
func (c *Conn) PlanStatus(ctx context.Context, runID string, sinceSeq int64) (*PlanStatus, error) {
	url := c.base + "/plan/status?run_id=" + runID + "&since_seq=" + strconv.FormatInt(sinceSeq, 10)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrPlanUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("agent plan status HTTP %d: %s", resp.StatusCode, string(msg))
	}
	var out PlanStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("failed to parse agent response: %w", err)
	}
	return &out, nil
}

// CancelPlan 请求中止。
func (c *Conn) CancelPlan(ctx context.Context, runID string) error {
	body, _ := json.Marshal(map[string]any{"run_id": runID})
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/plan/cancel", bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("agent plan cancel HTTP %d: %s", resp.StatusCode, string(msg))
	}
	return nil
}
