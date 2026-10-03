package web

// 审查修复回归测试（各项对应 review 清单）：
//   - run 的 selector 落库记实际执行集合（作用域裁剪后）；
//   - retireAgent 对 TLS 主机不做明文回退；
//   - /api/exec 重复 host_ids 去重（不再误报 409）；
//   - permsOf 查询出错不写负缓存；
//   - advertiseBase 端口部分校验；
//   - POST /api/hosts 名字与导入同口径；
//   - decodeJSON 超限 413 分流；
//   - 授权变更审计带 scopes 摘要；
//   - 升级端点 GetHost 的 404/500 分流；
//   - handleCreateUser 的 UserByName 错误分流；
//   - truncate 的 UTF-8 边界回退。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"wdp/internal/fmtutil"
	"wdp/internal/store"
)

// ---- run selector 记实际执行集合 ----

// TestRunSelectorRecordsTrimmedHosts 作用域裁剪后，run 落库的 selector 必须
// 记实际执行的主机集合（hosts + ids），而不是裁剪前的原始请求选择器——
// 否则事后审计无法还原真实触达范围；裁剪发生时审计附注原始选择器。
func TestRunSelectorRecordsTrimmedHosts(t *testing.T) {
	s, st := newAppServer(t, 18821) // 台账里已有 e2e-local（e2e-pool，明文回环 agent）
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")

	// 裁剪阈值用的第二台主机：不在执行者授权池内
	farID, err := st.CreateHost(&store.Host{
		Name: "selrec-far", Address: "127.0.0.1", AgentPort: 1,
		Pools: []string{"far-pool"}, AllowPlaintext: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var hosts []*store.Host
	hosts, _ = st.ListHosts("")
	var e2eID int64
	for _, hh := range hosts {
		if hh.Name == "e2e-local" {
			e2eID = hh.ID
		}
	}

	appID := uploadChartAs(t, h, admin, "selrec", "1.0.0")
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/apps/%d/scope", appID), map[string]any{
		"pools": []string{"e2e-pool"},
	}, &admin); rec.Code != http.StatusOK {
		t.Fatalf("设置应用 scope 应 200: %d %s", rec.Code, rec.Body)
	}

	scoped := newUserSession(t, h, admin, "selrec", "scoped-Pass1", "operator")
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/users/%d/scopes", userIDByName(t, st, "selrec")), map[string]any{
		"scopes": []map[string]any{{"verb": "run:execute", "kind": "pool", "value": "e2e-pool"}},
	}, &admin); rec.Code != http.StatusOK {
		t.Fatalf("追加授权应 200: %d %s", rec.Code, rec.Body)
	}

	// selector=all 命中两台，裁剪后只剩 e2e-local
	rec := do(t, h, "POST", "/api/runs", map[string]any{
		"items":    []map[string]any{{"app_id": appID, "version": "1.0.0"}},
		"selector": map[string]any{"kind": "all"},
	}, &scoped)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("范围内执行应 202: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		RunIDs []int64 `json:"run_ids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil || len(started.RunIDs) == 0 {
		t.Fatalf("解析 run_ids 失败: %v %s", err, rec.Body)
	}
	run, err := st.GetRun(started.RunIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"ids":[%d],"kind":"hosts"}`, e2eID)
	if run.Selector != want {
		t.Fatalf("selector 应记裁剪后实际执行集合 %s， got %s", want, run.Selector)
	}
	if strings.Contains(run.Selector, "far-pool") || strings.Contains(run.Selector, `"all"`) {
		t.Fatalf("selector 不得残留原始请求选择器: %s", run.Selector)
	}
	if farID == 0 {
		t.Fatal("far host 未建")
	}

	// 裁剪审计附注原始选择器（admin 可查审计）
	rec = do(t, h, "GET", "/api/audit?limit=50", nil, &admin)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "原始选择器") {
		t.Fatalf("裁剪审计应附注原始选择器: %d %s", rec.Code, rec.Body)
	}
}

// ---- retireAgent 不对 TLS 主机明文回退 ----

// TestRetireAgentNoPlaintextFallbackForTLSHosts mTLS 失败后，只有台账声明
// 明文的主机才允许明文兜底；TLS 主机记 warning、退役失败但不阻断删除
// （明文桩在线时旧实现会经明文"成功"退役，等于对 TLS 主机开明文通道）。
func TestRetireAgentNoPlaintextFallbackForTLSHosts(t *testing.T) {
	st := newEnrollOnlyStore(t)
	dataDir := t.TempDir()
	s, err := New(st, Options{AdminUser: "admin", DataDir: dataDir, CADir: filepath.Join(dataDir, "ca")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.cam == nil {
		t.Fatal("测试前提：CA 就绪（cam 非空）")
	}
	// 明文 HTTP 桩：任何方法都 200（模拟手工添加的回环 agent）
	stub := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	stub.Listener = mustListen(t, 18823)
	stub.Start()
	t.Cleanup(stub.Close)

	// TLS 主机（默认 AllowPlaintext=false）与明文主机指向同一明文端口
	tlsID, err := st.CreateHost(&store.Host{Name: "tls-h", Address: "127.0.0.1", AgentPort: 18823})
	if err != nil {
		t.Fatal(err)
	}
	plainID, err := st.CreateHost(&store.Host{Name: "plain-h", Address: "127.0.0.1", AgentPort: 18823, AllowPlaintext: true})
	if err != nil {
		t.Fatal(err)
	}
	token := loginSession(t, s)
	h := s.Handler()

	rec := do(t, h, "DELETE", fmt.Sprintf("/api/hosts/%d", tlsID), nil, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("TLS 主机删除不应被阻断: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"agent_retired":true`) || !strings.Contains(rec.Body.String(), "warning") {
		t.Fatalf("TLS 主机 mTLS 失败不得明文兜底成功（应带 warning 且 retired=false）: %s", rec.Body)
	}

	rec = do(t, h, "DELETE", fmt.Sprintf("/api/hosts/%d", plainID), nil, &token)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"agent_retired":true`) {
		t.Fatalf("明文主机应照常明文兜底退役: %d %s", rec.Code, rec.Body)
	}
}

// ---- /api/exec 重复 host_ids ----

// TestExecDuplicateHostIDs 重复提交同一主机 ID 不应被闸门误判为"被其它
// 执行占用"（同一把锁二次 TryLock 恒失败）→ 3s 后 409。去重后应正常执行。
func TestExecDuplicateHostIDs(t *testing.T) {
	s, st := newAppServer(t, 18825)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")
	var hosts []*store.Host
	hosts, _ = st.ListHosts("")
	if len(hosts) == 0 {
		t.Fatal("测试前提：台账已有主机")
	}
	id := hosts[0].ID

	rec := do(t, h, "POST", "/api/exec", map[string]any{
		"host_ids": []int64{id, id, id},
		"script":   "echo dup",
	}, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("重复 host_ids 应正常执行而非 409: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		RunID int64 `json:"run_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// 重复 ID 应去重为单台执行：后台收尾后任务明细只有一行；run 的
	// selector 同样只记去重后的集合
	run := waitExecRun(t, st, resp.RunID)
	tasks, err := st.RunTasks(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("重复 ID 应去重为单台执行: %+v", tasks)
	}
	want := fmt.Sprintf(`{"ids":[%d],"kind":"hosts"}`, id)
	if run.Selector != want {
		t.Fatalf("exec run selector 应为 %s, got %s", want, run.Selector)
	}
}

// ---- permsOf 负缓存 ----

// TestPermsOfNoNegativeCacheOnDBError UserByName 瞬时 DB 错误产生的空权限
// 视图不得写入 permCache（否则该用户直到缓存失效/重启恒 403）。
func TestPermsOfNoNegativeCacheOnDBError(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	admin := loginSession(t, s)
	newUserSession(t, h, admin, "negc", "scoped-Pass1", "viewer")
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/users/%d/scopes", userIDByName(t, st, "negc")), map[string]any{
		"scopes": []map[string]any{{"verb": "host:view", "kind": "pool", "value": "p1"}},
	}, &admin); rec.Code != http.StatusOK {
		t.Fatalf("追加授权应 200: %d %s", rec.Code, rec.Body)
	}

	// 正常路径：视图带授权
	p := s.permsOf("negc")
	if !p.canVerb(verbHostView) {
		t.Fatal("正常路径应有 host:view")
	}

	// 失效缓存后关闭 DB 模拟瞬时故障：空视图返回但不落缓存
	s.invalidatePerms("negc")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	p2 := s.permsOf("negc")
	if p2.canVerb(verbHostView) {
		t.Fatal("DB 错误时应返回空权限视图（fail closed）")
	}
	if _, cached := s.permCache["negc"]; cached {
		t.Fatal("查询出错的空视图不得写入 permCache（负缓存）")
	}
}

// ---- advertiseBase 端口校验 ----

// TestAdvertiseBasePortValidation Host 头的端口部分进入 curl | sudo sh
// 提示串：非数字/超界（可含引号等元字符）必须拒绝，host 部分校验管不到。
func TestAdvertiseBasePortValidation(t *testing.T) {
	s, _ := newTestServer(t)
	base := func(host string) error {
		r := httptest.NewRequest("POST", "/api/enroll-tokens", nil)
		r.Host = host
		_, err := s.advertiseBase(r)
		return err
	}
	for _, bad := range []string{
		`wdp.example.com:'8443'`, // 引号混入端口
		"wdp.example.com:8a",     // 非数字
		"wdp.example.com:99999",  // 超界
		"wdp.example.com:0",      // 超界
	} {
		if err := base(bad); err == nil || !strings.Contains(err.Error(), "invalid Host header") {
			t.Fatalf("%q 的端口部分应被拒绝: %v", bad, err)
		}
	}
	// 合法端口应通过端口校验（卡在后续明文检查——与端口无关）
	if err := base("wdp.example.com:8443"); err == nil || !strings.Contains(err.Error(), "plaintext") {
		t.Fatalf("合法端口不应被拒于 Host 校验: %v", err)
	}
}

// ---- POST /api/hosts 名字校验 ----

// TestCreateHostNameValidation 台账名与 CSV 导入/纳管同口径（safeName
// 字符集）：含 "/"、空格等字符直接 400，不再裸解码入库。
func TestCreateHostNameValidation(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	for _, bad := range []string{"bad/name", "with space", "semi;colon", "quote\"name"} {
		rec := do(t, h, "POST", "/api/hosts", map[string]any{
			"name": bad, "address": "10.0.0.99", "agent_port": 7602,
		}, &token)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("非法名字 %q 应 400: %d %s", bad, rec.Code, rec.Body)
		}
	}
	rec := do(t, h, "POST", "/api/hosts", map[string]any{
		"name": "good-name_1.x", "address": "10.0.0.99", "agent_port": 7602,
	}, &token)
	if rec.Code != http.StatusCreated {
		t.Fatalf("合法名字应 201: %d %s", rec.Code, rec.Body)
	}
}

// ---- decodeJSON 超限 413 ----

// TestDecodeJSONSizeLimit413 普通 JSON 端点超限应回 413（与
// decodeJSONLarge/上传同口径），而不是 400 "invalid JSON body"。
func TestDecodeJSONSizeLimit413(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)
	rec := do(t, h, "POST", "/api/hosts", map[string]any{
		"name": "big", "address": "10.0.0.99", "labels": strings.Repeat("x", 1<<20),
	}, &token)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限请求体应 413: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "exceeds") {
		t.Fatalf("413 文案应说明超限: %s", rec.Body)
	}
}

// ---- 授权变更审计 ----

// TestSetUserScopesAuditDetail 授权审计必须能还原"谁给谁加了什么"：
// detail 带 verb@kind:value 摘要（kind 空 = @all），空列表 = 清空全部。
func TestSetUserScopesAuditDetail(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	admin := loginSession(t, s)
	_ = newUserSession(t, h, admin, "audituser", "scoped-Pass1", "viewer")
	uid := userIDByName(t, st, "audituser")

	rec := do(t, h, "PUT", fmt.Sprintf("/api/users/%d/scopes", uid), map[string]any{
		"scopes": []map[string]any{
			{"verb": "host:edit", "kind": "pool", "value": "pool-a"},
			{"verb": "host:view", "kind": "label", "value": "env"},
			{"verb": "run:execute", "kind": "", "value": ""},
		},
	}, &admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("设置 scopes 应 200: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "GET", "/api/audit?limit=10", nil, &admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("查审计应 200: %d %s", rec.Code, rec.Body)
	}
	for _, want := range []string{
		"host:edit@pool:pool-a", "host:view@label:env", "run:execute@all",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("审计 detail 应包含 %s: %s", want, rec.Body)
		}
	}

	// 清空全部
	rec = do(t, h, "PUT", fmt.Sprintf("/api/users/%d/scopes", uid), map[string]any{
		"scopes": []map[string]any{},
	}, &admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空 scopes 应 200: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "GET", "/api/audit?limit=10", nil, &admin)
	if !strings.Contains(rec.Body.String(), "清空全部") {
		t.Fatalf("清空审计应可辨认: %s", rec.Body)
	}
}

// ---- 升级端点 404/500 分流 ----

// TestUpgradeHostDBErrorNot404 GetHost 的 DB 错误不得一律 404（把 500 掩盖
// 成"主机不存在"误导运维）。
func TestUpgradeHostDBErrorNot404(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	admin := loginSession(t, s)
	id, err := st.CreateHost(&store.Host{Name: "up-h", Address: "127.0.0.1", AgentPort: 1})
	if err != nil {
		t.Fatal(err)
	}
	// operator 有 host:upgrade 全局权限；先请求一次让权限视图进缓存，
	// 关库后 requirePerm 才不会因同一故障先回 403/500
	op := newUserSession(t, h, admin, "upop", "scoped-Pass1", "operator")
	if rec := do(t, h, "GET", "/api/exec/targets", nil, &op); rec.Code != http.StatusOK {
		t.Fatalf("预热权限缓存应 200: %d %s", rec.Code, rec.Body)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, "POST", fmt.Sprintf("/api/hosts/%d/upgrade", id), map[string]any{"force": false}, &op)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("DB 错误应 500 而非 404: %d %s", rec.Code, rec.Body)
	}
}

// ---- handleCreateUser 的 UserByName 错误分流 ----

// TestCreateUserDBErrorSplit UserByName 的非 NotFound 错误是 DB 故障：
// 不得当"不存在"继续创建（要么撞唯一约束、要么误报建成）。
func TestCreateUserDBErrorSplit(t *testing.T) {
	s, st := newTestServer(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/users", strings.NewReader(
		`{"name":"ghost","password":"password8","role":"viewer"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxUser{}, "admin"))
	rec := httptest.NewRecorder()
	s.handleCreateUser(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("DB 错误应 500 而非继续创建: %d %s", rec.Code, rec.Body)
	}
}

// ---- truncate 的 UTF-8 边界 ----

// TestTruncateUTF8Boundary 截断点落在多字节字符中间时必须回退到序列
// 边界（结果恒为合法 UTF-8）；边界已在字符尾时不得多退。
func TestTruncateUTF8Boundary(t *testing.T) {
	s := strings.Repeat("中", 4) + "abc" // 12 字节中文 + 3 字节 ASCII
	mark := "\n... (truncated)"         // 截断标记本身不计入 max（与旧实现一致）
	cases := []struct {
		max  int
		want string
	}{
		{0, mark},
		{1, mark},
		{2, mark},
		{3, "中" + mark}, // 边界正好：不得多退
		{4, "中" + mark}, // lead 后：回退到 3
		{5, "中" + mark}, // 续字节中：回退到 3
		{6, "中中" + mark},
		{12, strings.Repeat("中", 4) + mark},
		{13, strings.Repeat("中", 4) + "a" + mark},
		{15, s}, // 未超限原样返回
	}
	for _, c := range cases {
		got := truncate(s, c.max)
		if got != c.want {
			t.Fatalf("truncate(max=%d) = %q, want %q", c.max, got, c.want)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("truncate(max=%d) 产生非法 UTF-8: %q", c.max, got)
		}
	}
	// 纯截断辅助：任意切点的结果前缀均为合法 UTF-8
	for max := 0; max <= len(s); max++ {
		if !utf8.ValidString(fmtutil.TruncateUTF8(s, max)) {
			t.Fatalf("TruncateUTF8(max=%d) 产生非法 UTF-8", max)
		}
	}
}
