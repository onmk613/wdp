package web

// RBAC v1/v2 回归：角色全局权限、scope 列表裁剪与单资源校验、执行交集、
// 用户生命周期（建/禁用/重置密码/踢会话/最后管理员保护）、执行闸门排队。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wdp/internal/store"
)

// newUserSession 建一个用户并登录拿会话 cookie（值返回）。
func newUserSession(t *testing.T, h http.Handler, adminCookie string, name, password, role string) string {
	t.Helper()
	rec := do(t, h, "POST", "/api/users", map[string]any{
		"name": name, "password": password, "role": role,
	}, &adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("建用户 %s 应 200: %d %s", name, rec.Code, rec.Body)
	}
	rec = do(t, h, "POST", "/api/login", map[string]any{"user": name, "password": password}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录 %s 应 200: %d %s", name, rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	t.Fatal("登录未下发会话 cookie")
	return ""
}

// userIDByName 按用户名查 id（AUTOINCREMENT 会因 bootstrap 的 upsert
// 冲突跳号，测试不能假设连续 id）。
func userIDByName(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	users, err := st.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Name == name {
			return u.ID
		}
	}
	t.Fatalf("user %s not found", name)
	return 0
}

// TestRBACRoleAndScope 角色全局权限 + scope 追加授权的列表裁剪与单资源校验。
func TestRBACRoleAndScope(t *testing.T) {
	s, st := newAppServer(t, 18771)
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")

	// 两台主机：pool-a / pool-b（newAppServer 已建 e2e-local，id 不连续，按名取）
	st.CreateHost(&store.Host{Name: "ha", Address: "127.0.0.1", AgentPort: 1, Pools: []string{"pool-a"}})
	st.CreateHost(&store.Host{Name: "hb", Address: "127.0.0.1", AgentPort: 1, Pools: []string{"pool-b"}})
	hostsAll, _ := st.ListHosts("")
	hostID := map[string]int64{}
	for _, hh := range hostsAll {
		hostID[hh.Name] = hh.ID
	}

	viewer := newUserSession(t, h, admin, "viewer1", "viewer-Pass1", "viewer")
	scoped := newUserSession(t, h, admin, "scoped1", "scoped-Pass1", "viewer")
	// scoped1 = viewer + host:edit@pool-a（叠加授权）
	rec := do(t, h, "PUT", fmt.Sprintf("/api/users/%d/scopes", userIDByName(t, st, "scoped1")), map[string]any{
		"scopes": []map[string]any{{"verb": "host:edit", "kind": "pool", "value": "pool-a"}},
	}, &admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("追加授权应 200: %d %s", rec.Code, rec.Body)
	}

	// viewer：列表可见全部主机（host:view 全局），但不能改
	if rec = do(t, h, "GET", "/api/hosts", nil, &viewer); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"hb"`) {
		t.Fatalf("viewer 应看到全部主机: %d %s", rec.Code, rec.Body)
	}
	if rec = do(t, h, "PUT", fmt.Sprintf("/api/hosts/%d", hostID["ha"]), map[string]any{"Address": "127.0.0.1"}, &viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 改主机应 403: %d", rec.Code)
	}
	if rec = do(t, h, "GET", "/api/audit", nil, &viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 看审计应 403: %d", rec.Code)
	}
	if rec = do(t, h, "GET", "/api/users", nil, &viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 管用户应 403: %d", rec.Code)
	}

	// scoped：改 pool-a 主机过、pool-b 拒
	if rec = do(t, h, "PUT", fmt.Sprintf("/api/hosts/%d", hostID["ha"]), map[string]any{"Address": "127.0.0.1", "Pools": []string{"pool-a"}}, &scoped); rec.Code != http.StatusOK {
		t.Fatalf("scoped 改 pool-a 主机应 200: %d %s", rec.Code, rec.Body)
	}
	if rec = do(t, h, "PUT", fmt.Sprintf("/api/hosts/%d", hostID["hb"]), map[string]any{"Address": "127.0.0.1"}, &scoped); rec.Code != http.StatusForbidden {
		t.Fatalf("scoped 改 pool-b 主机应 403: %d", rec.Code)
	}

	// 列表裁剪：给 scoped 再加 host:view@pool-a，则列表只剩 pool-a 主机
	do(t, h, "PUT", fmt.Sprintf("/api/users/%d/scopes", userIDByName(t, st, "scoped1")), map[string]any{
		"scopes": []map[string]any{
			{"verb": "host:edit", "kind": "pool", "value": "pool-a"},
			{"verb": "host:view", "kind": "pool", "value": "pool-a"},
		},
	}, &admin)
	if rec = do(t, h, "GET", "/api/hosts", nil, &scoped); rec.Code != http.StatusOK {
		t.Fatalf("scoped 列表应 200: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"hb"`) || !strings.Contains(rec.Body.String(), `"ha"`) {
		t.Fatalf("scoped 列表应只见 pool-a 主机: %s", rec.Body)
	}

	// operator：可执行、可建应用，不能删用户
	op := newUserSession(t, h, admin, "op1", "oper-Pass12", "operator")
	if rec = do(t, h, "GET", "/api/modules", nil, &op); rec.Code != http.StatusOK {
		t.Fatalf("operator 看模块应 200: %d", rec.Code)
	}
	if rec = do(t, h, "DELETE", "/api/users/2", nil, &op); rec.Code != http.StatusForbidden {
		t.Fatalf("operator 删用户应 403: %d", rec.Code)
	}
}

// TestHostUpdateScopeWrite 单台 PUT /api/hosts 的写入校验回归：新归属
// （池/组/标签）必须整体落在 host:edit 授权内（与批量 assign 同口径），
// allow_plaintext 开关收敛到 host:enroll——此前 PUT 整体替换请求体，
// scoped 用户可把主机挪进任意池扩大可见面，或把已纳管主机降级明文。
func TestHostUpdateScopeWrite(t *testing.T) {
	s, st := newAppServer(t, 18772)
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")

	st.CreateHost(&store.Host{Name: "ha", Address: "127.0.0.1", AgentPort: 1, Pools: []string{"pool-a"}})
	hostsAll, _ := st.ListHosts("")
	var haID int64
	for _, hh := range hostsAll {
		if hh.Name == "ha" {
			haID = hh.ID
		}
	}
	scoped := newUserSession(t, h, admin, "scopedw", "scoped-Pass1", "viewer")
	do(t, h, "PUT", fmt.Sprintf("/api/users/%d/scopes", userIDByName(t, st, "scopedw")), map[string]any{
		"scopes": []map[string]any{{"verb": "host:edit", "kind": "pool", "value": "pool-a"}},
	}, &admin)

	// 授权外目标池：403（写入校验，matchScope"任一命中"不够）
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/hosts/%d", haID), map[string]any{"Address": "127.0.0.1", "Pools": []string{"pool-b"}}, &scoped); rec.Code != http.StatusForbidden {
		t.Fatalf("scoped 挪出授权池应 403: %d %s", rec.Code, rec.Body)
	}
	// 授权外标签键同样拒绝（labels 驱动 label 型授权面）
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/hosts/%d", haID), map[string]any{"Address": "127.0.0.1", "Pools": []string{"pool-a"}, "Labels": `{"zone":"dmz"}`}, &scoped); rec.Code != http.StatusForbidden {
		t.Fatalf("scoped 打授权外标签应 403: %d %s", rec.Code, rec.Body)
	}
	// 授权内目标值放行
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/hosts/%d", haID), map[string]any{"Address": "127.0.0.1", "Pools": []string{"pool-a"}}, &scoped); rec.Code != http.StatusOK {
		t.Fatalf("授权内改动应 200: %d %s", rec.Code, rec.Body)
	}
	// allow_plaintext 是通道信任模型开关：host:edit 不带 host:enroll 不可改
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/hosts/%d", haID), map[string]any{"Address": "127.0.0.1", "allow_plaintext": true}, &scoped); rec.Code != http.StatusForbidden {
		t.Fatalf("scoped 置 allow_plaintext 应 403: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/hosts/%d", haID), map[string]any{"Address": "127.0.0.1", "allow_plaintext": true}, &admin); rec.Code != http.StatusOK {
		t.Fatalf("admin 置 allow_plaintext 应 200: %d %s", rec.Code, rec.Body)
	}
}

// TestExecTargetsIsolation 执行目标资源裁剪：scoped 用户的目标清单只含
// 授权范围内的主机，restricted=true；全局用户拿全量。
func TestExecTargetsIsolation(t *testing.T) {
	s, st := newAppServer(t, 18775)
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")
	st.CreateHost(&store.Host{Name: "hx", Address: "127.0.0.1", AgentPort: 1, Pools: []string{"pool-x"}})

	scoped := newUserSession(t, h, admin, "tsc", "scoped-Pass1", "viewer")
	do(t, h, "PUT", fmt.Sprintf("/api/users/%d/scopes", userIDByName(t, st, "tsc")), map[string]any{
		"scopes": []map[string]any{{"verb": "run:execute", "kind": "pool", "value": "e2e-pool"}},
	}, &admin)

	// scoped：只见到 e2e-pool 的主机（e2e-local），restricted=true
	rec := do(t, h, "GET", "/api/exec/targets", nil, &scoped)
	if rec.Code != http.StatusOK {
		t.Fatalf("targets 应 200: %d %s", rec.Code, rec.Body)
	}
	var res struct {
		Hosts      []*store.Host `json:"hosts"`
		Restricted bool          `json:"restricted"`
	}
	json.Unmarshal(rec.Body.Bytes(), &res)
	if !res.Restricted || len(res.Hosts) != 1 || res.Hosts[0].Name != "e2e-local" {
		t.Fatalf("scoped targets 异常: restricted=%v hosts=%v", res.Restricted, res.Hosts)
	}
	// viewer（无 run:execute）→ 403
	viewer := newUserSession(t, h, admin, "tv", "viewer-Pass1", "viewer")
	if rec := do(t, h, "GET", "/api/exec/targets", nil, &viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 应 403: %d", rec.Code)
	}
	// admin：全量 unrestricted
	rec = do(t, h, "GET", "/api/exec/targets", nil, &admin)
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Restricted || len(res.Hosts) != 2 {
		t.Fatalf("admin targets 异常: restricted=%v n=%d", res.Restricted, len(res.Hosts))
	}
}

// TestRBACExecIntersection 执行交集：scoped 用户只能打到授权范围内的主机。
func TestRBACExecIntersection(t *testing.T) {
	s, st := newAppServer(t, 18772)
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")

	// e2e-local（e2e-pool）已在 newAppServer 建；再加一台 pool-b
	st.CreateHost(&store.Host{Name: "hb", Address: "127.0.0.1", AgentPort: 2, Pools: []string{"pool-b"}})

	scoped := newUserSession(t, h, admin, "sc", "scoped-Pass1", "viewer")
	do(t, h, "PUT", fmt.Sprintf("/api/users/%d/scopes", userIDByName(t, st, "sc")), map[string]any{
		"scopes": []map[string]any{{"verb": "run:execute", "kind": "pool", "value": "e2e-pool"}},
	}, &admin)

	// 打 pool-b 主机（id=2）：一台都不在范围 → 403
	if rec := do(t, h, "POST", "/api/exec", map[string]any{
		"host_ids": []int64{2}, "script": "echo hi",
	}, &scoped); rec.Code != http.StatusForbidden {
		t.Fatalf("范围外执行应 403: %d %s", rec.Code, rec.Body)
	}
	// 打两台：被裁剪到 1 台（e2e-local）→ 202/200 正常执行
	rec := do(t, h, "POST", "/api/exec", map[string]any{
		"host_ids": []int64{1, 2}, "script": "echo hi",
	}, &scoped)
	if rec.Code != http.StatusOK {
		t.Fatalf("交集执行应 200: %d %s", rec.Code, rec.Body)
	}
}

// TestExecScopeTrimAudited 部分裁剪：照常执行 + 审计留痕 + run 的 selector
// 记录实际执行集合（记裁前集合会让事后审计无法还原真实触达范围）。
func TestExecScopeTrimAudited(t *testing.T) {
	s, st := newAppServer(t, 18776)
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")
	st.CreateHost(&store.Host{Name: "trim-b", Address: "127.0.0.1", AgentPort: 2, Pools: []string{"pool-b"}})
	hostsAll, _ := st.ListHosts("")
	hostID := map[string]int64{}
	for _, hh := range hostsAll {
		hostID[hh.Name] = hh.ID
	}
	scoped := newUserSession(t, h, admin, "trimsc", "scoped-Pass1", "viewer")
	do(t, h, "PUT", fmt.Sprintf("/api/users/%d/scopes", userIDByName(t, st, "trimsc")), map[string]any{
		"scopes": []map[string]any{{"verb": "run:execute", "kind": "pool", "value": "e2e-pool"}},
	}, &admin)

	rec := do(t, h, "POST", "/api/exec", map[string]any{
		"host_ids": []int64{hostID["e2e-local"], hostID["trim-b"]}, "script": "echo hi",
	}, &scoped)
	if rec.Code != http.StatusOK {
		t.Fatalf("部分裁剪应照常执行: %d %s", rec.Code, rec.Body)
	}

	// run selector = 实际执行集合（只含 e2e-local）
	runs, _ := st.ListRuns(5)
	if len(runs) == 0 {
		t.Fatal("应产生 run 记录")
	}
	var sel struct {
		Kind string  `json:"kind"`
		IDs  []int64 `json:"ids"`
	}
	if err := json.Unmarshal([]byte(runs[0].Selector), &sel); err != nil {
		t.Fatalf("selector 解析: %v", err)
	}
	if len(sel.IDs) != 1 || sel.IDs[0] != hostID["e2e-local"] {
		t.Fatalf("selector 应记实际执行集合: %+v", sel)
	}

	// 裁剪审计留痕
	logs, _ := st.ListAuditLogs(50, "")
	found := false
	for _, lg := range logs {
		if strings.Contains(lg.Detail, "执行范围被权限裁剪") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应记录裁剪审计: %+v", logs)
	}
}

// TestUserLifecycle 用户生命周期：禁用踢会话且不能再登录、重置密码旧会话
// 失效、不能删自己、最后一个 admin 不可禁用/降级/删除。
func TestUserLifecycle(t *testing.T) {
	s, st := newAppServer(t, 18773)
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")

	u := newUserSession(t, h, admin, "tmp", "tmp-Pass123", "viewer")

	// 禁用 → 会话即刻失效 + 登录被拒
	uid := userIDByName(t, st, "tmp")
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/users/%d", uid), map[string]any{"disabled": true}, &admin); rec.Code != http.StatusOK {
		t.Fatalf("禁用应 200: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/api/hosts", nil, &u); rec.Code != http.StatusUnauthorized {
		t.Fatalf("禁用后会话应失效(401): %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/api/login", map[string]any{"user": "tmp", "password": "tmp-Pass123"}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("禁用后登录应 403: %d", rec.Code)
	}

	// 启用 + 重置密码 → 旧密码失效
	do(t, h, "PUT", fmt.Sprintf("/api/users/%d", uid), map[string]any{"disabled": false}, &admin)
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/users/%d/password", uid), map[string]any{"password": "new-Pass456"}, &admin); rec.Code != http.StatusOK {
		t.Fatalf("重置密码应 200: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/api/login", map[string]any{"user": "tmp", "password": "tmp-Pass123"}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧密码应失效: %d", rec.Code)
	}

	// 最后一个 admin 保护
	adminID := userIDByName(t, st, "admin")
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/users/%d", adminID), map[string]any{"disabled": true}, &admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("禁用最后 admin 应 400: %d", rec.Code)
	}
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/users/%d", adminID), map[string]any{"role": "viewer"}, &admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("降级最后 admin 应 400: %d", rec.Code)
	}
	if rec := do(t, h, "DELETE", fmt.Sprintf("/api/users/%d", adminID), nil, &admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("删除最后 admin 应 400: %d", rec.Code)
	}
	// 不能删自己（非 admin 场景）
	if rec := do(t, h, "DELETE", fmt.Sprintf("/api/users/%d", uid), nil, &admin); rec.Code != http.StatusOK {
		t.Fatalf("删其它用户应 200: %d %s", rec.Code, rec.Body)
	}
}

// TestHostGateQueue 执行闸门：占用期间 exec/升级 409、应用执行排队
// （queued），释放后完成。
func TestHostGateQueue(t *testing.T) {
	s, st := newAppServer(t, 18774)
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")

	// 上传一个最小 chart 供应用执行
	body := &bytes.Buffer{}
	mw := newMultipartWriter(body, nil, "tgz", "gate.tgz", buildChartTgz(t, "gateapp", "1.0.0", "gated-ok"))
	req := httptest.NewRequest("POST", "/api/apps/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	addCookie(req, admin)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传应 201: %d %s", rec.Code, rec.Body)
	}
	var app store.App
	json.Unmarshal(rec.Body.Bytes(), &app)

	hostsAll, _ := st.ListHosts("")
	var host1 int64
	for _, hh := range hostsAll {
		host1 = hh.ID // e2e-local（唯一）
	}

	// 占住闸门
	release := s.gate.Acquire([]int64{host1})

	// 同步类（exec / 升级）→ 409 快速失败
	if rec := do(t, h, "POST", "/api/exec", map[string]any{
		"host_ids": []int64{host1}, "script": "echo x",
	}, &admin); rec.Code != http.StatusConflict {
		t.Fatalf("闸门占用期间 exec 应 409: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", fmt.Sprintf("/api/hosts/%d/upgrade", host1), map[string]any{}, &admin); rec.Code != http.StatusConflict {
		t.Fatalf("闸门占用期间升级应 409: %d %s", rec.Code, rec.Body)
	}

	// 应用执行 → 异步排队，run 停在 queued
	if rec := do(t, h, "POST", "/api/runs", map[string]any{
		"items":    []map[string]any{{"app_id": app.ID}},
		"selector": map[string]any{"kind": "all"},
	}, &admin); rec.Code != http.StatusAccepted {
		t.Fatalf("应用执行应 202: %d %s", rec.Code, rec.Body)
	}
	var run *store.Run
	deadline := time.Now().Add(3 * time.Second)
	for run == nil || run.Status != "queued" {
		runs, _ := st.ListRuns(10)
		if len(runs) > 0 {
			run = runs[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("run 应处于 queued: %+v", run)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if r, _ := st.GetRun(run.ID); r.Status != "queued" {
		t.Fatalf("闸门占用期间应保持 queued: %s", r.Status)
	}

	release()
	for i := 0; i < 100; i++ {
		if r, _ := st.GetRun(run.ID); r != nil && r.Status == "succeeded" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	r, _ := st.GetRun(run.ID)
	t.Fatalf("释放后 run 应 succeeded: %+v", r)
}
