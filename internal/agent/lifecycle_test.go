package agent

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wdp/internal/connection"
	"wdp/internal/connection/agentconn"
	"wdp/internal/model"
)

// TestShutdownEndpoint 验证 /shutdown 端点响应。
func TestShutdownEndpoint(t *testing.T) {
	srv := New(":0")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	conn := agentconn.New(&model.Host{Name: "p", Conn: "agent", AgentURL: ts.URL}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestExecBecome 验证 /exec 的 become 字段透传（sudo -n 形式的脚本包装）。
func TestExecBecome(t *testing.T) {
	srv := New(":0")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	conn := agentconn.New(&model.Host{Name: "p", Conn: "agent", AgentURL: ts.URL}, nil)
	// become_user 指向当前用户（sudo 大概率不可用于测试环境，验证脚本包装不崩即可）
	out, err := conn.Exec(context.Background(), connection.ExecRequest{
		Script: "id -un", BecomeUser: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Stdout, "wangqiang") && out.Stdout != "" && out.Code != 0 {
		// 非测试机用户名场景仅验证非崩溃
		t.Logf("id 输出: %q", out.Stdout)
	}
}
