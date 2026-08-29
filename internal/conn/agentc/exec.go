package agentc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"wdp/internal/conn"
	"wdp/internal/model"
)

// Exec 调用 agent 的 /exec。
func (c *Conn) Exec(ctx context.Context, req conn.ExecRequest) (conn.ExecResult, error) {
	becomeUser, becomePW := "", ""
	if req.BecomeUser != "" {
		becomeUser = req.BecomeUser
		becomePW = model.Secret(c.host.BecomePassword, c.host.BecomePasswordEnv)
		// 明文 http:// 时提权密码裸奔于网络（无 TLS 加密）：明确告警，
		// 对外纳管应配置 mTLS（README 已知限制；告警避免无意识踩坑）
		if becomePW != "" && strings.HasPrefix(c.base, "http://") {
			warnPlaintextBecome()
		}
	}
	body, _ := json.Marshal(map[string]any{
		"script":          req.Script,
		"stdin":           req.Stdin,
		"env":             req.Env,
		"timeout_ms":      req.TimeoutMs,
		"become_user":     becomeUser,
		"become_password": becomePW,
	})
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/exec", bytes.NewReader(body))
	if err != nil {
		return conn.ExecResult{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(hreq)
	if err != nil {
		return conn.ExecResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return conn.ExecResult{}, fmt.Errorf("agent exec HTTP %d: %s", resp.StatusCode, string(msg))
	}
	var out struct {
		Code      int    `json:"code"`
		Stdout    string `json:"stdout"`
		Stderr    string `json:"stderr"`
		TimedOut  bool   `json:"timed_out"`
		Cancelled bool   `json:"cancelled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return conn.ExecResult{}, fmt.Errorf("failed to parse agent response: %w", err)
	}
	if out.TimedOut {
		return conn.ExecResult{Code: out.Code, Stdout: out.Stdout, Stderr: out.Stderr},
			context.DeadlineExceeded
	}
	if out.Cancelled {
		// 控制端主动取消/断开 ≠ 超时，错误归因分开（此前一律报 DeadlineExceeded）
		return conn.ExecResult{Code: out.Code, Stdout: out.Stdout, Stderr: out.Stderr},
			context.Canceled
	}
	return conn.ExecResult{Code: out.Code, Stdout: out.Stdout, Stderr: out.Stderr}, nil
}

// warnPlaintextBecome 明文 http + become 密码的一次性告警（每进程一条，
// 避免大规模主机重复刷屏）。
var warnPlaintextBecome = func() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			fmt.Fprintln(os.Stderr, "[warn] become password is being sent over plain HTTP (no TLS); sniffable on the network. Configure mTLS for the agent.")
		})
	}
}()
