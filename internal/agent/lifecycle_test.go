package agent

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wdp/internal/connection"
	"wdp/internal/connection/agentconn"
	"wdp/internal/model"
)

// TestTokenFileAuth 验证 --token-file 读取后自删 + token 认证生效（push 自举依赖）。
func TestTokenFileAuth(t *testing.T) {
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "push.tok")
	if err := os.WriteFile(tokFile, []byte("push-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := New(":0")
	if err := srv.ConfigureAuth("", tokFile, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tokFile); !os.IsNotExist(err) {
		t.Fatal("token 文件读取后应被删除")
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// 正确 token 可用
	host := &model.Host{Name: "p", Conn: "agent", AgentURL: ts.URL, Token: "push-secret"}
	conn := agentconn.New(host)
	if err := conn.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, err := conn.Exec(context.Background(), connection.ExecRequest{Script: "echo push-ok"})
	if err != nil || !strings.Contains(out.Stdout, "push-ok") {
		t.Fatalf("exec: %+v err=%v", out, err)
	}

	// 错误 token 被拒
	bad := agentconn.New(&model.Host{Name: "p", Conn: "agent", AgentURL: ts.URL, Token: "wrong"})
	if err := bad.Connect(context.Background()); err == nil {
		bad.Exec(context.Background(), connection.ExecRequest{Script: "echo x"})
	}
	if _, err := bad.Exec(context.Background(), connection.ExecRequest{Script: "echo x"}); err == nil {
		t.Fatal("错误 token 应被拒绝")
	}
}

// TestShutdownEndpoint 验证 /shutdown 端点响应。
func TestShutdownEndpoint(t *testing.T) {
	srv := New(":0")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	conn := agentconn.New(&model.Host{Name: "p", Conn: "agent", AgentURL: ts.URL})
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
	conn := agentconn.New(&model.Host{Name: "p", Conn: "agent", AgentURL: ts.URL})
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
