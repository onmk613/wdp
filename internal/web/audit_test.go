package web

// 操作审计与执行记录关联用户：登录/登出/增删改动作落 audit_logs，
// runs.user 记录触发者；GET /api/audit 可按关键字过滤。

import (
	"net/http"
	"strings"
	"testing"
)

func TestAuditAndRunUser(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()

	// 登录失败与成功都留痕
	if rec := do(t, h, "POST", "/api/login", map[string]string{"user": "admin", "password": "wrong"}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误凭据应 401: %d", rec.Code)
	}
	token := loginSession(t, s)

	// 增：手动建档 → create host
	if rec := do(t, h, "POST", "/api/hosts", map[string]any{"name": "audit-h1", "address": "127.0.0.1", "agent_port": 1}, &token); rec.Code != http.StatusCreated {
		t.Fatalf("建档: %d %s", rec.Code, rec.Body)
	}
	// 改：更新
	if rec := do(t, h, "PUT", "/api/hosts/1", map[string]any{"address": "10.7.0.2"}, &token); rec.Code != http.StatusOK {
		t.Fatalf("更新: %d", rec.Code)
	}
	// exec：run 应带 user
	if rec := do(t, h, "POST", "/api/exec", map[string]any{"host_ids": []int64{1}, "script": "echo hi", "timeout_sec": 1}, &token); rec.Code != http.StatusOK {
		t.Fatalf("exec: %d %s", rec.Code, rec.Body)
	}
	run, err := st.GetRun(1)
	if err != nil || run.User != "admin" {
		t.Fatalf("run 应关联 admin: %+v err=%v", run, err)
	}
	// 删：run 删除
	if rec := do(t, h, "DELETE", "/api/runs/1", nil, &token); rec.Code != http.StatusOK {
		t.Fatalf("删 run: %d", rec.Code)
	}

	logs, err := st.ListAuditLogs(50, "")
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, a := range logs {
		joined += a.Action + "|" + a.Object + "|" + a.User + "\n"
	}
	for _, want := range []string{"login|session|admin", "login_failed|session|admin", "create|host|admin", "update|host|admin", "delete|run|admin"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("审计缺 %q:\n%s", want, joined)
		}
	}
	// 过滤
	logs, _ = st.ListAuditLogs(50, "host")
	if len(logs) == 0 {
		t.Fatal("按 host 过滤应有结果")
	}
	_ = http.StatusOK
}
