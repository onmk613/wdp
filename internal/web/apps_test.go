package web

// 应用上传与执行 E2E：进程内起一个 loopback agent（无认证模式），上传
// 最小 chart tgz，按 hosts 选择器执行，验证 runs/run_tasks 落库与结果。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"wdp/internal/agent"
	"wdp/internal/ca"
	"wdp/internal/chart"
	"wdp/internal/console"
	"wdp/internal/store"
)

// buildChartTgz 构造最小 chart：chart.yaml + deploy.yaml（一个 shell 任务）。
func buildChartTgz(t *testing.T, name, version, marker string) []byte {
	t.Helper()
	tasks := fmt.Sprintf("- name: e2e\n  hosts: all\n  tasks:\n    - name: say\n      shell: 'echo %s'\n", marker)
	return buildChartTgzFull(t, name, version, "", tasks, "{}")
}

// buildChartTgzFull 构造带 helpers/自定义任务的 chart（回归 include 等模板能力）。
func buildChartTgzFull(t *testing.T, name, version, helpers, tasksYAML, valuesYAML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(name, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write("chart.yaml", fmt.Sprintf("name: %s\nversion: %s\ndescription: minimal e2e chart\nrequired: []\n", name, version))
	if helpers != "" {
		write("_helpers.tpl", helpers)
	}
	write("deploy.yaml", tasksYAML)
	write("values.yaml", valuesYAML)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildChartTgzPhases 构造带额外相位文件的 chart（deploy.yaml 固定打
// deploy-marker，其余 <相位名>.yaml 用给定任务 YAML）。
func buildChartTgzPhases(t *testing.T, name, version string, phaseFiles map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(name, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write("chart.yaml", fmt.Sprintf("name: %s\nversion: %s\ndescription: phaseful e2e chart\nrequired: []\n", name, version))
	write("deploy.yaml", "- name: e2e\n  hosts: all\n  tasks:\n    - name: say\n      shell: 'echo deploy-marker'\n")
	write("values.yaml", "{}")
	for phase, y := range phaseFiles {
		write(phase+".yaml", y)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestVersionScopeUpdate 版本作用域就地变更（不升版本）：latest 版本变更
// 同步应用级；非 latest 只动该版本行；未知版本 404。
func TestVersionScopeUpdate(t *testing.T) {
	s, st := newAppServer(t, 18767)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	up := func(version string) *store.App {
		t.Helper()
		body := &bytes.Buffer{}
		mw := newMultipartWriter(body, nil, "tgz", "scoped.tgz", buildChartTgz(t, "scoped", version, "x"))
		req := httptest.NewRequest("POST", "/api/apps/upload", body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		addCookie(req, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("上传 %s 应 201: %d %s", version, rec.Code, rec.Body)
		}
		var app store.App
		json.Unmarshal(rec.Body.Bytes(), &app)
		return &app
	}
	app := up("1.0.0")
	up("1.0.1") // 1.0.1 成为 latest

	put := func(body map[string]any) *httptest.ResponseRecorder {
		rec := do(t, h, "PUT", fmt.Sprintf("/api/apps/%d/scope", app.ID), body, &token)
		return rec
	}
	versionScope := func(version string) (pools, groups []string, labels string) {
		t.Helper()
		p, g, l, ok, err := st.VersionScopes(app.ID, version)
		if err != nil || !ok {
			t.Fatalf("读 %s 版本 scope 失败: %v %v", version, ok, err)
		}
		return p, g, l
	}

	// 1. 改非 latest（1.0.0）：只动该版本行，应用级不动
	rec := put(map[string]any{
		"version": "1.0.0", "pools": []string{"legacy-pool"}, "groups": []string{"legacy-group"},
		"labels": `{"tier":"legacy"}`,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("作用域变更应 200: %d %s", rec.Code, rec.Body)
	}
	p, g, l := versionScope("1.0.0")
	if len(p) != 1 || p[0] != "legacy-pool" || len(g) != 1 || g[0] != "legacy-group" || l != `{"tier":"legacy"}` {
		t.Fatalf("1.0.0 版本 scope 异常: %v %v %v", p, g, l)
	}
	if a, _ := st.GetApp(app.ID); len(a.Pools) != 0 || len(a.Groups) != 0 || a.Labels != "{}" {
		t.Fatalf("改非 latest 不应动应用级: %+v", a)
	}

	// 2. 改 latest（version 缺省 = latest）：应用级同步
	rec = put(map[string]any{
		"pools": []string{"prod-pool"}, "groups": []string{"prod-group"}, "labels": `{"tier":"web"}`,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("latest 作用域变更应 200: %d %s", rec.Code, rec.Body)
	}
	p, g, l = versionScope("1.0.1")
	if len(p) != 1 || p[0] != "prod-pool" || l != `{"tier":"web"}` {
		t.Fatalf("1.0.1 版本 scope 异常: %v %v %v", p, g, l)
	}
	if a, _ := st.GetApp(app.ID); len(a.Pools) != 1 || a.Pools[0] != "prod-pool" || a.Labels != `{"tier":"web"}` {
		t.Fatalf("改 latest 应同步应用级: %+v", a)
	}
	// 1.0.0 不被波及
	if p, _, _ = versionScope("1.0.0"); len(p) != 1 || p[0] != "legacy-pool" {
		t.Fatalf("1.0.0 scope 不应被 latest 变更波及: %v", p)
	}

	// 3. 未知版本 404
	if rec = put(map[string]any{"version": "9.9.9", "pools": []string{"x"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("未知版本应 404: %d %s", rec.Code, rec.Body)
	}
}

// TestRunItemPerPhaseE2E 行级相位：同一应用两次入清单各执行不同相位、
// 版本元数据带相位清单、未知相位预校验拒绝。
func TestRunItemPerPhaseE2E(t *testing.T) {
	s, st := newAppServer(t, 18766)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	// 1. 上传带 status 相位的 chart
	tgz := buildChartTgzPhases(t, "phaseful", "1.0.0", map[string]string{
		"status": "- name: st\n  hosts: all\n  tasks:\n    - name: say\n      shell: 'echo status-marker'\n",
	})
	body := &bytes.Buffer{}
	mw := newMultipartWriter(body, nil, "tgz", "phaseful.tgz", tgz)
	req := httptest.NewRequest("POST", "/api/apps/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	addCookie(req, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传应 201: %d %s", rec.Code, rec.Body)
	}
	var app store.App
	json.Unmarshal(rec.Body.Bytes(), &app)

	// 2. 版本元数据带相位清单（deploy 在前）
	rec = do(t, h, "GET", fmt.Sprintf("/api/apps/%d", app.ID), nil, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("应用详情应 200: %d %s", rec.Code, rec.Body)
	}
	var detail struct {
		Versions []*store.AppVersion `json:"versions"`
	}
	json.Unmarshal(rec.Body.Bytes(), &detail)
	if len(detail.Versions) != 1 || len(detail.Versions[0].Phases) != 2 ||
		detail.Versions[0].Phases[0] != "deploy" || detail.Versions[0].Phases[1] != "status" {
		t.Fatalf("版本应带相位清单 [deploy status]: %s", rec.Body)
	}

	// 3. 同一应用两次入清单：第一行 deploy（缺省），第二行 status
	rec = do(t, h, "POST", "/api/runs", map[string]any{
		"items": []map[string]any{
			{"app_id": app.ID},
			{"app_id": app.ID, "phase": "status"},
		},
		"selector": map[string]any{"kind": "pool", "value": "e2e-pool"},
	}, &token)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("执行应 202: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		RunIDs []int64 `json:"run_ids"`
	}
	json.Unmarshal(rec.Body.Bytes(), &started)
	if len(started.RunIDs) != 2 {
		t.Fatalf("应产生两条 run: %s", rec.Body)
	}
	waitRun := func(id int64) *store.Run {
		t.Helper()
		var run *store.Run
		for i := 0; i < 100; i++ {
			run, _ = st.GetRun(id)
			if run != nil && run.Status != "running" && run.Status != "queued" {
				return run
			}
			time.Sleep(100 * time.Millisecond)
		}
		return run
	}
	r0, r1 := waitRun(started.RunIDs[0]), waitRun(started.RunIDs[1])
	if r0 == nil || r0.Status != "succeeded" || r0.Phase != "deploy" {
		t.Fatalf("第一行应 deploy 成功: %+v", r0)
	}
	if r1 == nil || r1.Status != "succeeded" || r1.Phase != "status" {
		t.Fatalf("第二行应 status 成功: %+v", r1)
	}
	tasks, _ := st.RunTasks(started.RunIDs[1])
	if len(tasks) == 0 || !strings.Contains(tasks[0].Detail, "status-marker") {
		t.Fatalf("status 相位任务应输出 status-marker: %+v", tasks)
	}

	// 4. 未知相位 → 预校验拒绝
	rec = do(t, h, "POST", "/api/runs", map[string]any{
		"items":    []map[string]any{{"app_id": app.ID, "phase": "backup"}},
		"selector": map[string]any{"kind": "pool", "value": "e2e-pool"},
	}, &token)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "has no phase") {
		t.Fatalf("未知相位应 400（has no phase）: %d %s", rec.Code, rec.Body)
	}
}

// newAppServerMTLS 构造带纳管信任链的 server + 进程内 mTLS agent 主机：
// 与控制台装出来的 agent 同形态（https + ctl 客户端证书），回归问题 1/5
// （server→agent 连接必须带 mTLS 材料）。
func newAppServerMTLS(t *testing.T, agentPort int) (*Server, *store.Store) {
	t.Helper()
	dataDir := t.TempDir()
	caDir := filepath.Join(dataDir, "ca")
	if _, _, _, err := ca.Init(ca.InitOptions{Dir: caDir, Days: 3650}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ca.Issue(ca.IssueOptions{Dir: caDir, Profile: ca.ProfileClient}, "ctl"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ca.Issue(ca.IssueOptions{
		Dir: caDir, CACertPath: filepath.Join(caDir, "ca.crt"), CAKeyPath: filepath.Join(caDir, "ca.key"),
		SANs: []string{"127.0.0.1"},
	}, "agent-test"); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dataDir, "wdp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreateUser("admin", "$2a$10$x", "admin"); err != nil && !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatal(err)
	}
	s, err := New(st, Options{
		AdminUser: "admin", AdminPass: "e2e-pass-1",
		DataDir: dataDir, CADir: caDir,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(fmt.Sprintf("127.0.0.1:%d", agentPort))
	if err := ag.ConfigureAuth(
		filepath.Join(caDir, "ca.crt"),
		filepath.Join(caDir, "agent-test.crt"),
		filepath.Join(caDir, "agent-test.key"),
	); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(ag.Handler())
	srv.Listener = mustListen(t, agentPort)
	srv.TLS = ag.MTLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	if _, err := st.CreateHost(&store.Host{Name: "mtls-local", Address: "127.0.0.1", AgentPort: agentPort, Pools: []string{"e2e-pool"}}); err != nil {
		t.Fatal(err)
	}
	return s, st
}

// newAppServer 构造带数据目录的 server + 一个进程内 loopback agent 主机。
func newAppServer(t *testing.T, agentPort int) (*Server, *store.Store) {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "wdp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreateUser("admin", "$2a$10$x", "admin"); err != nil && !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatal(err)
	}
	s, err := New(st, Options{
		AdminUser: "admin", AdminPass: "e2e-pass-1",
		DataDir: dataDir,
		CADir:   filepath.Join(dataDir, "ca"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 进程内 agent（loopback 无认证模式，Handler 由 httptest 托管在随机端口；
	// 台账主机地址固定 127.0.0.1:agentPort → 用 httptest.NewUnstartedServer
	// 固定监听该端口）
	ag := agent.New(fmt.Sprintf("127.0.0.1:%d", agentPort))
	srv := httptest.NewUnstartedServer(ag.Handler())
	srv.Listener = mustListen(t, agentPort)
	srv.Start()
	t.Cleanup(srv.Close)
	// 该 fixture 起的是明文 loopback agent（未配 mTLS）：台账必须显式声明
	// allow_plaintext——CA 启用时控制台默认按 mTLS 建连，不做静默降级
	if _, err := st.CreateHost(&store.Host{Name: "e2e-local", Address: "127.0.0.1", AgentPort: agentPort, Pools: []string{"e2e-pool"}, AllowPlaintext: true}); err != nil {
		t.Fatal(err)
	}
	return s, st
}

// TestAppUploadAndRunE2E 上传（版本取自 chart.yaml）→执行→runs 校验。
func TestAppUploadAndRunE2E(t *testing.T) {
	s, st := newAppServer(t, 18761)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	// 1. 上传 chart：应用名与版本取自 chart.yaml（probe@1.0.0）
	body := &bytes.Buffer{}
	mw := newMultipartWriter(body, nil, "tgz", "probe.tgz", buildChartTgz(t, "probe", "1.0.0", "hello-from-chart"))
	req := httptest.NewRequest("POST", "/api/apps/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	addCookie(req, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传应 201: %d %s", rec.Code, rec.Body)
	}
	var app store.App
	json.Unmarshal(rec.Body.Bytes(), &app)
	if app.ID == 0 || app.Name != "probe" || app.LatestVersion != "1.0.0" || app.VersionCount != 1 {
		t.Fatalf("应用元数据异常（版本应取自 chart.yaml）: %s", rec.Body)
	}

	// 2. 同版本重传 → 拒绝（版本一经发布不可覆盖）
	body = &bytes.Buffer{}
	mw = newMultipartWriter(body, nil, "tgz", "probe.tgz", buildChartTgz(t, "probe", "1.0.0", "overwritten-marker"))
	req = httptest.NewRequest("POST", "/api/apps/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	addCookie(req, token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "不可覆盖") {
		t.Fatalf("同版本重传应被拒（不可覆盖）: %d %s", rec.Code, rec.Body)
	}
	app2, _ := st.GetApp(app.ID)
	if app2.VersionCount != 1 {
		t.Fatalf("拒绝后版本数不应变化: %+v", app2)
	}
	// 关键回归：被拒的重复上传不得毁掉已发布版本的制品
	//（曾因先 moveTgz 覆盖存档、再因版本重复删除该文件而丢版本）
	tgzOld, err := st.VersionTgz(app.ID, "1.0.0")
	if err != nil {
		t.Fatalf("已发布版本路径应仍在库中: %v", err)
	}
	if _, err := os.Stat(tgzOld); err != nil {
		t.Fatalf("已发布版本的制品文件不应被破坏: %v", err)
	}
	spec0, err := readChartSpecInTest(tgzOld)
	if err != nil {
		t.Fatalf("已发布版本应仍可加载: %v", err)
	}
	if !strings.Contains(spec0.DeployYAML, "hello-from-chart") {
		t.Fatalf("已发布版本内容不应被覆盖: %s", spec0.DeployYAML)
	}

	// 3. 新版本 → 版本数增、最新切换
	body = &bytes.Buffer{}
	mw = newMultipartWriter(body, nil, "tgz", "probe.tgz", buildChartTgz(t, "probe", "1.0.1", "v101"))
	req = httptest.NewRequest("POST", "/api/apps/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	addCookie(req, token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	app3, _ := st.GetApp(app.ID)
	if app3.VersionCount != 2 || app3.LatestVersion != "1.0.1" {
		t.Fatalf("新版本应入库为最新: %+v", app3)
	}

	// 4. 池归属 + 按池执行（多值）
	if _, err := st.BatchAssign([]int64{1}, []string{"e2e-pool"}, nil, true, false, nil, false); err != nil {
		t.Fatal(err)
	}
	rbody := `{"items":[{"app_id":` + fmt.Sprint(app.ID) + `}],"selector":{"kind":"pool","value":"e2e-pool"}}`
	req = httptest.NewRequest("POST", "/api/runs", strings.NewReader(rbody))
	req.Header.Set("Content-Type", "application/json")
	addCookie(req, token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("执行应 202: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		RunIDs []int64 `json:"run_ids"`
	}
	json.Unmarshal(rec.Body.Bytes(), &started)
	if len(started.RunIDs) != 1 {
		t.Fatalf("run_ids 异常: %s", rec.Body)
	}

	// 5. 轮询 run 完成（最新版本 1.0.1）
	var run *store.Run
	for i := 0; i < 100; i++ {
		run, _ = st.GetRun(started.RunIDs[0])
		if run != nil && run.Status != "running" && run.Status != "queued" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if run == nil || run.Status != "succeeded" || run.Version != "1.0.1" {
		ts, _ := st.RunTasks(started.RunIDs[0])
		for _, x := range ts {
			t.Logf("TASK %s/%s@%s status=%s detail=%s", x.Play, x.Task, x.Host, x.Status, x.Detail)
		}
		t.Fatalf("run 应成功且版本为 chart.yaml 版本: %+v", run)
	}
	tasks, _ := st.RunTasks(started.RunIDs[0])
	if len(tasks) == 0 || tasks[0].Status != "ok" || !strings.Contains(tasks[0].Detail, "v101") {
		t.Fatalf("任务结果异常: %+v", tasks)
	}

	// 6. 向既有应用上传 chart.yaml 名称不符 → 400
	body = &bytes.Buffer{}
	mw = newMultipartWriter(body, nil, "tgz", "other.tgz", buildChartTgz(t, "other-app", "1.0.0", "x"))
	req = httptest.NewRequest("POST", fmt.Sprintf("/api/apps/%d/versions/upload", app.ID), body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	addCookie(req, token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "does not match") {
		t.Fatalf("名称不符应 400: %d %s", rec.Code, rec.Body)
	}

	// 7. 版本删除 + 最新顶替
	versions, _ := st.ListVersions(app.ID)
	for _, v := range versions {
		if v.Version == "1.0.1" {
			req = httptest.NewRequest("DELETE", fmt.Sprintf("/api/apps/%d/versions/%d", app.ID, v.ID), nil)
			req.Header.Set("Content-Type", "application/json") // 空 CT 的变更请求被 requireAuth 拒绝
			addCookie(req, token)
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("删版本应 200: %d %s", rec.Code, rec.Body)
			}
		}
	}
	app4, _ := st.GetApp(app.ID)
	if app4.VersionCount != 1 || app4.LatestVersion != "1.0.0" {
		t.Fatalf("删最新后应顶替: %+v", app4)
	}

	// 8. 执行校验失败路径
	rbody = `{"items":[{"app_id":` + fmt.Sprint(app.ID) + `,"version":"nope"}],"selector":{"kind":"all"}}`
	req = httptest.NewRequest("POST", "/api/runs", strings.NewReader(rbody))
	req.Header.Set("Content-Type", "application/json")
	addCookie(req, token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知版本应 400: %d %s", rec.Code, rec.Body)
	}
}

// TestAppSpecRoundtrip 图形化编辑器：新建 spec → 读回 → 改内容保存为新版本。
func TestAppSpecRoundtrip(t *testing.T) {
	s, st := newAppServer(t, 18763)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	// 新建（图形化：版本/步骤/文件/作用域）
	create := map[string]any{
		"name": "graph-app", "version": "2.0.0", "description": "图形化创建",
		"files": []map[string]any{
			{"path": "chart.yaml", "content": "name: graph-app\nversion: 2.0.0\n"},
			{"path": "values.yaml", "content": "greeting: hi\n"},
			{"path": "deploy.yaml", "content": "- name: graph-app\n  hosts: all\n  tasks:\n    - name: say\n      shell: echo {{ .greeting }}\n"},
			{"path": "files/note.txt", "content": "hello file"},
		},
		"pools": []string{"e2e-pool"},
	}
	rec := do(t, h, "POST", "/api/apps/spec", create, &token)
	if rec.Code != http.StatusCreated {
		t.Fatalf("新建 spec 应 201: %d %s", rec.Code, rec.Body)
	}
	var app store.App
	json.Unmarshal(rec.Body.Bytes(), &app)
	if app.Name != "graph-app" || app.LatestVersion != "2.0.0" || len(app.Pools) != 1 {
		t.Fatalf("新建结果异常: %s", rec.Body)
	}
	// 重名新建拒绝（提示走修改）
	if rec := do(t, h, "POST", "/api/apps/spec", create, &token); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("重名应 400: %d %s", rec.Code, rec.Body)
	}

	// 读回 spec：含 deploy/values/文件
	rec = do(t, h, "GET", fmt.Sprintf("/api/apps/%d/spec", app.ID), nil, &token)
	var spec AppSpec
	json.Unmarshal(rec.Body.Bytes(), &spec)
	if spec.Version != "2.0.0" || !strings.Contains(spec.DeployYAML, "echo {{ .greeting }}") {
		t.Fatalf("spec 读回异常: %s", rec.Body)
	}
	found := false
	for _, f := range spec.Files {
		if f.Path == "files/note.txt" && f.Content != nil && *f.Content == "hello file" {
			found = true
		}
	}
	if !found {
		t.Fatalf("spec 应含上传的文件: %+v", spec.Files)
	}

	// 修改并保存为新版本（改号）
	save := map[string]any{
		"version": "2.1.0", "description": "改过的",
		"files": []map[string]any{
			{"path": "chart.yaml", "content": "name: graph-app\nversion: 2.1.0\n"},
			{"path": "values.yaml", "content": "greeting: hello-v2\n"},
			{"path": "deploy.yaml", "content": "- name: graph-app\n  hosts: all\n  tasks:\n    - name: say2\n      shell: echo changed\n"},
			{"path": "files/extra.txt", "content": "extra"},
		},
		"base_version": "2.0.0",
	}
	rec = do(t, h, "PUT", fmt.Sprintf("/api/apps/%d/spec", app.ID), save, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存 spec 应 200: %d %s", rec.Code, rec.Body)
	}
	app2, _ := st.GetApp(app.ID)
	if app2.VersionCount != 2 || app2.LatestVersion != "2.1.0" {
		t.Fatalf("保存后元数据异常: %+v", app2)
	}
	rec = do(t, h, "GET", fmt.Sprintf("/api/apps/%d/spec", app.ID), nil, &token)
	json.Unmarshal(rec.Body.Bytes(), &spec)
	if spec.Version != "2.1.0" || !strings.Contains(spec.DeployYAML, "changed") {
		t.Fatalf("新版本内容异常: %s", rec.Body)
	}
	// 底本文件保留 + 新文件加入
	paths := map[string]bool{}
	for _, f := range spec.Files {
		paths[f.Path] = true
	}
	if !paths["files/note.txt"] || !paths["files/extra.txt"] {
		t.Fatalf("文件应保留底本并合并新增: %+v", spec.Files)
	}

	// 同号保存 → 拒绝（修改必须产出新版本，版本不可覆盖）
	save["version"] = "2.1.0"
	for _, f := range save["files"].([]map[string]any) {
		if f["path"] == "chart.yaml" {
			f["content"] = "name: graph-app\nversion: 2.1.0\n"
		}
		if f["path"] == "deploy.yaml" {
			f["content"] = "- name: graph-app\n  hosts: all\n  tasks:\n    - name: say3\n      shell: echo overwritten\n"
		}
	}
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/apps/%d/spec", app.ID), save, &token); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "不可覆盖") {
		t.Fatalf("同号保存应被拒（不可覆盖）: %d %s", rec.Code, rec.Body)
	}
	app3, _ := st.GetApp(app.ID)
	if app3.VersionCount != 2 || app3.LatestVersion != "2.1.0" {
		t.Fatalf("拒绝后元数据不应变化: %+v", app3)
	}
	// 版本 2.1.0 内容保持首次保存的样子（未被覆盖）
	rec = do(t, h, "GET", fmt.Sprintf("/api/apps/%d/spec", app.ID), nil, &token)
	json.Unmarshal(rec.Body.Bytes(), &spec)
	if strings.Contains(spec.DeployYAML, "overwritten") {
		t.Fatalf("已发布版本内容不应被覆盖: %s", rec.Body)
	}
	// 再改一次（新版本号）→ 正常成为最新
	save["version"] = "2.2.0"
	for _, f := range save["files"].([]map[string]any) {
		if f["path"] == "chart.yaml" {
			f["content"] = "name: graph-app\nversion: 2.2.0\n"
		}
	}
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/apps/%d/spec", app.ID), save, &token); rec.Code != http.StatusOK {
		t.Fatalf("新版本号保存应 200: %d %s", rec.Code, rec.Body)
	}
	app4, _ := st.GetApp(app.ID)
	if app4.VersionCount != 3 || app4.LatestVersion != "2.2.0" {
		t.Fatalf("新版本应生效: %+v", app4)
	}

	// 非法版本号拒绝
	save["version"] = "../evil"
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/apps/%d/spec", app.ID), save, &token); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法版本应 400: %d %s", rec.Code, rec.Body)
	}
}

// TestVersionScopeInheritance scope 是版本级的：编辑器按底本版本加载
// 池/组/标签，保存即继承到底本之上的新版本；应用级 = 最近保存的 scope。
func TestVersionScopeInheritance(t *testing.T) {
	s, st := newAppServer(t, 18763)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	create := map[string]any{
		"name": "scope-app", "version": "1.0.0", "description": "",
		"files": []map[string]any{
			{"path": "chart.yaml", "content": "name: scope-app\nversion: 1.0.0\n"},
			{"path": "values.yaml", "content": "a: 1\n"},
			{"path": "deploy.yaml", "content": "- name: s\n  hosts: all\n  tasks:\n    - name: t\n      shell: 'true'\n"},
		},
		"pools": []string{"pool-a"}, "groups": []string{"grp-a"}, "labels": `{"env":"prod"}`,
	}
	rec := do(t, h, "POST", "/api/apps/spec", create, &token)
	if rec.Code != http.StatusCreated {
		t.Fatalf("新建应 201: %d %s", rec.Code, rec.Body)
	}
	var app store.App
	json.Unmarshal(rec.Body.Bytes(), &app)

	// 基于 1.0.0 保存 2.0.0，scope 改为 pool-b
	save := map[string]any{
		"version": "2.0.0", "description": "",
		"files": []map[string]any{
			{"path": "chart.yaml", "content": "name: scope-app\nversion: 2.0.0\n"},
			{"path": "values.yaml", "content": "a: 2\n"},
			{"path": "deploy.yaml", "content": "- name: s\n  hosts: all\n  tasks:\n    - name: t\n      shell: 'true'\n"},
		},
		"base_version": "1.0.0",
		"pools":        []string{"pool-b"}, "groups": []string{}, "labels": `{"env":"dev"}`,
	}
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/apps/%d/spec", app.ID), save, &token); rec.Code != http.StatusOK {
		t.Fatalf("保存应 200: %d %s", rec.Code, rec.Body)
	}

	getSpec := func(version string) AppSpec {
		var spec AppSpec
		u := fmt.Sprintf("/api/apps/%d/spec", app.ID)
		if version != "" {
			u += "?version=" + version
		}
		rec := do(t, h, "GET", u, nil, &token)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET spec(%s) 应 200: %d %s", version, rec.Code, rec.Body)
		}
		json.Unmarshal(rec.Body.Bytes(), &spec)
		return spec
	}

	// 默认（最新 2.0.0）：本次保存的 scope
	latest := getSpec("")
	if len(latest.Pools) != 1 || latest.Pools[0] != "pool-b" || latest.Labels != `{"env":"dev"}` {
		t.Fatalf("最新版本 scope 异常: %+v %s", latest.Pools, latest.Labels)
	}
	// 底本 1.0.0：仍是他自己的 scope（编辑它 → 编辑器预填 pool-a/grp-a）
	base := getSpec("1.0.0")
	if len(base.Pools) != 1 || base.Pools[0] != "pool-a" || len(base.Groups) != 1 || base.Groups[0] != "grp-a" || base.Labels != `{"env":"prod"}` {
		t.Fatalf("底本版本 scope 应按版本返回: %+v %+v %s", base.Pools, base.Groups, base.Labels)
	}
	// 应用级 = 最近保存
	a, _ := st.GetApp(app.ID)
	if len(a.Pools) != 1 || a.Pools[0] != "pool-b" {
		t.Fatalf("应用级 scope 应跟随最近保存: %+v", a.Pools)
	}
}

// TestExecAndRunMTLS 回归问题 1/5：server→agent 连接必须带 mTLS 材料
// （远程执行与应用执行都要走 https+ctl 客户端证书）。
func TestExecAndRunMTLS(t *testing.T) {
	s, st := newAppServerMTLS(t, 18764)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	// 探活确认 mTLS 可达
	rec := do(t, h, "POST", "/api/hosts/1/probe", map[string]any{}, &token)
	var pr ProbeResult
	json.Unmarshal(rec.Body.Bytes(), &pr)
	if pr.Status != "online" || pr.Scheme != "https" {
		t.Fatalf("mTLS 探活应 online/https: %+v", pr)
	}

	// 远程执行（曾因明文 HTTP 对 TLS 监听返回 400）
	rec = do(t, h, "POST", "/api/exec", map[string]any{"host_ids": []int64{1}, "script": "echo mtls-exec-ok"}, &token)
	var er struct {
		OK      int              `json:"ok"`
		Failed  int              `json:"failed"`
		Results []ExecHostResult `json:"results"`
	}
	json.Unmarshal(rec.Body.Bytes(), &er)
	if er.OK != 1 || er.Results[0].Err != "" || !strings.Contains(er.Results[0].Stdout, "mtls-exec-ok") {
		t.Fatalf("mTLS 远程执行应成功: %s", rec.Body)
	}

	// 应用执行（曾经报 agent health check failed: HTTP 400）
	body := &bytes.Buffer{}
	mw := newMultipartWriter(body, nil, "tgz", "probe.tgz", buildChartTgz(t, "probe", "1.0.0", "mtls-app-ok"))
	req := httptest.NewRequest("POST", "/api/apps/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	addCookie(req, token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传应 201: %d %s", rec.Code, rec.Body)
	}
	var app store.App
	json.Unmarshal(rec.Body.Bytes(), &app)

	rec = do(t, h, "POST", "/api/runs", map[string]any{
		"items":    []map[string]any{{"app_id": app.ID}},
		"selector": map[string]any{"kind": "pool", "value": "e2e-pool"},
	}, &token)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("执行应 202: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		RunIDs []int64 `json:"run_ids"`
	}
	json.Unmarshal(rec.Body.Bytes(), &started)
	var run *store.Run
	for i := 0; i < 100; i++ {
		run, _ = st.GetRun(started.RunIDs[0])
		if run != nil && run.Status != "running" && run.Status != "queued" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if run == nil || run.Status != "succeeded" {
		tasks, _ := st.RunTasks(started.RunIDs[0])
		t.Fatalf("mTLS 应用执行应成功: %+v tasks=%+v", run, tasks)
	}
	tasks, _ := st.RunTasks(started.RunIDs[0])
	if len(tasks) == 0 || !strings.Contains(tasks[0].Detail, "mtls-app-ok") {
		t.Fatalf("应用执行输出异常: %+v", tasks)
	}
}

// TestExecAPIE2E 远程命令执行（进程内 agent）。
func TestExecAPIE2E(t *testing.T) {
	s, st := newAppServer(t, 18762)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	req := httptest.NewRequest("POST", "/api/exec", strings.NewReader(`{"host_ids":[1],"script":"echo remote-ok"}`))
	req.Header.Set("Content-Type", "application/json")
	addCookie(req, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("exec 应 200: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		RunID   int64            `json:"run_id"`
		OK      int              `json:"ok"`
		Results []ExecHostResult `json:"results"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.OK != 1 || len(resp.Results) != 1 || resp.Results[0].Code != 0 || !strings.Contains(resp.Results[0].Stdout, "remote-ok") {
		t.Fatalf("exec 结果异常: %s", rec.Body)
	}
	run, _ := st.GetRun(resp.RunID)
	if run == nil || run.Status != "succeeded" || run.Kind != "exec" {
		t.Fatalf("exec run 记录异常: %+v", run)
	}
	// 空 script 拒绝
	req = httptest.NewRequest("POST", "/api/exec", strings.NewReader(`{"host_ids":[1],"script":""}`))
	req.Header.Set("Content-Type", "application/json")
	addCookie(req, token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空脚本应 400: %d", rec.Code)
	}
}

// ---- 测试工具 ----

// loginSession2 以指定密码登录（newAppServer 的 bootstrap 密码）。
func loginSession2(t *testing.T, s *Server, password string) string {
	t.Helper()
	rec := do(t, s.Handler(), "POST", "/api/login", map[string]string{"user": "admin", "password": password}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login 应 200: %d %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	t.Fatal("无会话 cookie")
	return ""
}

// addCookie 注入会话。
func addCookie(req *http.Request, token string) {
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
}

// newMultipartWriter 构造 multipart 表单（文件 + 字段）。
func newMultipartWriter(buf *bytes.Buffer, fields map[string]string, fileField, fileName string, file []byte) *multipart.Writer {
	w := multipart.NewWriter(buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	fw, _ := w.CreateFormFile(fileField, fileName)
	_, _ = fw.Write(file)
	_ = w.Close()
	return w
}

// mustListen 在指定端口建 Listener（测试并发端口避让由调用方保证）。
func mustListen(t *testing.T, port int) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("listen %d: %v", port, err)
	}
	return ln
}

// readChartSpecInTest 直接读取 tgz 的 chart 内容（不经过 HTTP）。
func readChartSpecInTest(tgz string) (*AppSpec, error) {
	ch, err := chart.LoadWithLimits(tgz, chart.Limits{})
	if err != nil {
		return nil, err
	}
	defer ch.Close()
	return console.ReadSpecFromDir(ch.Dir, ch.Meta.Name, ch.Meta.Version, ch.Meta.Description)
}

// TestSetLatestAndBatchDelete 设默认版本（latest）与批量删除应用。
func TestSetLatestAndBatchDelete(t *testing.T) {
	s, st := newAppServer(t, 18765)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	// 建两个应用，各两个版本
	mk := func(name, v1, v2 string) int64 {
		body := &bytes.Buffer{}
		mw := newMultipartWriter(body, nil, "tgz", "a.tgz", buildChartTgz(t, name, v1, "x"))
		req := httptest.NewRequest("POST", "/api/apps/upload", body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		addCookie(req, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("上传 %s 应 201: %d %s", name, rec.Code, rec.Body)
		}
		var app store.App
		json.Unmarshal(rec.Body.Bytes(), &app)
		body2 := &bytes.Buffer{}
		mw2 := newMultipartWriter(body2, nil, "tgz", "a.tgz", buildChartTgz(t, name, v2, "y"))
		req2 := httptest.NewRequest("POST", fmt.Sprintf("/api/apps/%d/versions/upload", app.ID), body2)
		req2.Header.Set("Content-Type", mw2.FormDataContentType())
		addCookie(req2, token)
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusCreated {
			t.Fatalf("上传 %s@%s 应 201: %d %s", name, v2, rec2.Code, rec2.Body)
		}
		return app.ID
	}
	idA := mk("alpha", "1.0.0", "1.0.1")
	idB := mk("beta", "2.0.0", "2.0.1")

	// 默认版本自动是最新：把 alpha 的 1.0.0 设为默认
	vers, _ := st.ListVersions(idA)
	var oldVer *store.AppVersion
	for _, v := range vers {
		if v.Version == "1.0.0" {
			oldVer = v
		}
	}
	if oldVer == nil {
		t.Fatal("缺少 1.0.0")
	}
	rec := do(t, h, "POST", fmt.Sprintf("/api/apps/%d/versions/%d/latest", idA, oldVer.ID), map[string]any{}, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("设默认版本应 200: %d %s", rec.Code, rec.Body)
	}
	appA, _ := st.GetApp(idA)
	if appA.LatestVersion != "1.0.0" {
		t.Fatalf("默认版本应切到 1.0.0: %+v", appA)
	}
	// 不存在的版本 → 404
	if rec := do(t, h, "POST", fmt.Sprintf("/api/apps/%d/versions/99999/latest", idA), map[string]any{}, &token); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在版本应 404: %d", rec.Code)
	}

	// 批量删除两个应用
	rec = do(t, h, "POST", "/api/apps/batch", map[string]any{"ids": []int64{idA, idB}, "action": "delete"}, &token)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":2`) {
		t.Fatalf("批量删除应 ok=2: %d %s", rec.Code, rec.Body)
	}
	apps, _ := st.ListApps()
	if len(apps) != 0 {
		t.Fatalf("批量删除后应无应用: %+v", apps)
	}
	// 制品目录应被清空
	entries, _ := os.ReadDir(filepath.Join(s.opts.DataDir, "apps"))
	for _, e := range entries {
		sub, _ := os.ReadDir(filepath.Join(s.opts.DataDir, "apps", e.Name()))
		if len(sub) != 0 {
			t.Fatalf("应用制品应清理干净: %s 残留 %d 个文件", e.Name(), len(sub))
		}
	}
	// 未知 action 拒绝
	if rec := do(t, h, "POST", "/api/apps/batch", map[string]any{"ids": []int64{1}, "action": "boom"}, &token); rec.Code != http.StatusBadRequest {
		t.Fatalf("未知 action 应 400: %d", rec.Code)
	}
}

// TestChartHelpersAndValues 回归：chart 的 _helpers.tpl（include）与
// values.yaml 引用必须在 server 执行时可用——接线少了
// chart.Open（CollectHelpers 构建引擎）与合并 values 时，include 会报
// `no template "x" associated with template "wdp"`（node-exporter 实测踩过）。
func TestChartHelpersAndValues(t *testing.T) {
	s, st := newAppServer(t, 18766)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	helpers := `{{- define "demo.greeting" -}}hello-helper{{- end -}}
`
	tasks := `- name: helper-demo
  hosts: all
  tasks:
    - name: render helper + values
      shell: 'echo {{ include "demo.greeting" . }}-{{ .custom }}'
`
	body := &bytes.Buffer{}
	mw := newMultipartWriter(body, nil, "tgz", "demo.tgz", buildChartTgzFull(t, "helper-app", "1.0.0", helpers, tasks, "custom: from-values\n"))
	req := httptest.NewRequest("POST", "/api/apps/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	addCookie(req, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传应 201: %d %s", rec.Code, rec.Body)
	}
	var app store.App
	json.Unmarshal(rec.Body.Bytes(), &app)

	rec = do(t, h, "POST", "/api/runs", map[string]any{
		"items":    []map[string]any{{"app_id": app.ID}},
		"selector": map[string]any{"kind": "pool", "value": "e2e-pool"},
	}, &token)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("执行应 202: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		RunIDs []int64 `json:"run_ids"`
	}
	json.Unmarshal(rec.Body.Bytes(), &started)
	var run *store.Run
	for i := 0; i < 100; i++ {
		run, _ = st.GetRun(started.RunIDs[0])
		if run != nil && run.Status != "running" && run.Status != "queued" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	tasks0, _ := st.RunTasks(started.RunIDs[0])
	if run == nil || run.Status != "succeeded" {
		for _, x := range tasks0 {
			t.Logf("TASK %s@%s status=%s detail=%s", x.Task, x.Host, x.Status, x.Detail)
		}
		t.Fatalf("include 渲染应成功: run=%+v", run)
	}
	// values.yaml 为 {} → .custom 未定义应报错，说明 chart values 确实参与了渲染；
	// 这里断言的是 helper 生效（hello-helper 出现在输出）
	if len(tasks0) == 0 || !strings.Contains(tasks0[0].Detail, "hello-helper") {
		t.Fatalf("helper 输出应在结果中: %+v", tasks0)
	}
}

// TestEditorSavePreservesChart 回归：编辑器保存不得丢失 play 级属性与注释。
// 曾因结构化重写只保留 tasks，丢掉 play 级 become: true，导致
// node-exporter 创建用户报 "creating a user requires become: true"。
func TestEditorSavePreservesChart(t *testing.T) {
	s, _ := newAppServer(t, 18767)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	// chart 带注释 + play 级 become/serial + values（模拟 node-exporter 形态）
	helpers := ""
	deploy := `# 部署相位：创建运行账户需要 play 级提权
- name: app
  hosts: all
  become: true
  serial: 1
  tasks:
    - name: create user   # 需要 root
      user:
        name: appuser
        system: true
`
	body := &bytes.Buffer{}
	mw := newMultipartWriter(body, nil, "tgz", "app.tgz", buildChartTgzFull(t, "preserve-app", "1.0.0", helpers, deploy, "{}\n"))
	req := httptest.NewRequest("POST", "/api/apps/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	addCookie(req, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传应 201: %d %s", rec.Code, rec.Body)
	}
	var app store.App
	json.Unmarshal(rec.Body.Bytes(), &app)

	// 读回 spec：deploy.yaml 应逐字保留（含注释与 play 级键）
	rec = do(t, h, "GET", fmt.Sprintf("/api/apps/%d/spec", app.ID), nil, &token)
	var spec AppSpec
	json.Unmarshal(rec.Body.Bytes(), &spec)
	if !strings.Contains(spec.DeployYAML, "become: true") || !strings.Contains(spec.DeployYAML, "# 部署相位") {
		t.Fatalf("spec 读回应保留 play 级属性与注释:\n%s", spec.DeployYAML)
	}

	// 模拟"编辑器未动步骤即保存"（前端会原样回传 deploy_yaml）
	save := map[string]any{
		"version": "1.0.1", "description": spec.Description,
		"values_yaml": spec.ValuesYAML, "deploy_yaml": spec.DeployYAML,
		"base_version": "1.0.0",
	}
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/apps/%d/spec", app.ID), save, &token); rec.Code != http.StatusOK {
		t.Fatalf("保存应 200: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "GET", fmt.Sprintf("/api/apps/%d/spec", app.ID), nil, &token)
	var spec2 AppSpec
	json.Unmarshal(rec.Body.Bytes(), &spec2)
	if spec2.DeployYAML != spec.DeployYAML {
		t.Fatalf("保存后 deploy.yaml 应逐字不变:\n原:\n%s\n新:\n%s", spec.DeployYAML, spec2.DeployYAML)
	}
	if !strings.Contains(spec2.DeployYAML, "become: true") || !strings.Contains(spec2.DeployYAML, "serial: 1") {
		t.Fatalf("play 级 become/serial 不得丢失:\n%s", spec2.DeployYAML)
	}
}

// TestDeleteRun 执行记录删除（单条 + 批量），任务明细级联清理。
func TestDeleteRun(t *testing.T) {
	s, st := newAppServer(t, 18768)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	// 造两条 exec 记录
	ids := []int64{}
	for i := 0; i < 2; i++ {
		rec := do(t, h, "POST", "/api/exec", map[string]any{"host_ids": []int64{1}, "script": "echo x"}, &token)
		if rec.Code != http.StatusOK {
			t.Fatalf("exec 应 200: %d %s", rec.Code, rec.Body)
		}
		var resp struct {
			RunID int64 `json:"run_id"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		ids = append(ids, resp.RunID)
	}
	if tasks, _ := st.RunTasks(ids[0]); len(tasks) == 0 {
		t.Fatal("应有任务明细")
	}
	// 单删
	if rec := do(t, h, "DELETE", fmt.Sprintf("/api/runs/%d", ids[0]), nil, &token); rec.Code != http.StatusOK {
		t.Fatalf("删除 run 应 200: %d %s", rec.Code, rec.Body)
	}
	if _, err := st.GetRun(ids[0]); err == nil {
		t.Fatal("已删除的 run 不应可读")
	}
	if tasks, _ := st.RunTasks(ids[0]); len(tasks) != 0 {
		t.Fatalf("任务明细应级联删除: %+v", tasks)
	}
	if rec := do(t, h, "DELETE", fmt.Sprintf("/api/runs/%d", ids[0]), nil, &token); rec.Code != http.StatusNotFound {
		t.Fatalf("重复删除应 404: %d", rec.Code)
	}
	// 批量删
	rec := do(t, h, "POST", "/api/runs/batch", map[string]any{"ids": []int64{ids[1]}}, &token)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":1`) {
		t.Fatalf("批量删除应 ok=1: %d %s", rec.Code, rec.Body)
	}
	runs, _ := st.ListRuns(50)
	if len(runs) != 0 {
		t.Fatalf("批量删除后应无记录: %+v", runs)
	}
}

// TestChartMetaVerbatimRoundtrip chart.yaml 全字段（required/marker_dir/
// phases 等）随 verbatim 原文往返：IDE 提交 chart.yaml → 注释与元数据
// 逐字保留；部分保存（不提交 chart.yaml、升版本走生成路径）→ 底本元数据
// 合并进生成的 chart.yaml。
func TestChartMetaVerbatimRoundtrip(t *testing.T) {
	s, _ := newAppServer(t, 18763)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	deploy := "- name: s\n  hosts: all\n  tasks:\n    - name: t\n      shell: 'true'\n"
	chartV1 := `# meta comment
name: meta-app
version: 1.0.0
required: [app.port]
marker_dir: /var/lib/wdp2
check_mode: true
sensitive_values: [db.password]
phases:
  uninstall: {clears_marker: true}
  reload: {record: true, values_from: marker}
`
	create := map[string]any{
		"name": "meta-app", "version": "1.0.0", "description": "d",
		"files": []map[string]any{
			{"path": "chart.yaml", "content": chartV1},
			{"path": "values.yaml", "content": "app: {port: 8080}\n"},
			{"path": "deploy.yaml", "content": deploy},
		},
	}
	if rec := do(t, h, "POST", "/api/apps/spec", create, &token); rec.Code != http.StatusCreated {
		t.Fatalf("创建应 201: %d %s", rec.Code, rec.Body)
	}

	readChart := func(version string) string {
		t.Helper()
		var spec AppSpec
		rec := do(t, h, "GET", "/api/apps/1/spec?version="+version, nil, &token)
		if rec.Code != http.StatusOK {
			t.Fatalf("读 spec: %d %s", rec.Code, rec.Body)
		}
		json.Unmarshal(rec.Body.Bytes(), &spec)
		for _, f := range spec.Files {
			if f.Path == "chart.yaml" {
				return deref(f.Content)
			}
		}
		return ""
	}

	// verbatim 往返：注释与全部元数据逐字保留
	got := readChart("1.0.0")
	for _, want := range []string{"# meta comment", "marker_dir: /var/lib/wdp2", "check_mode: true",
		"sensitive_values: [db.password]", "clears_marker: true", "values_from: marker"} {
		if !strings.Contains(got, want) {
			t.Fatalf("chart.yaml 元数据/注释应逐字保留，缺 %q:\n%s", want, got)
		}
	}

	// 部分保存（不提交 chart.yaml、升版本）：生成路径合并底本元数据
	save := map[string]any{
		"version": "2.0.0", "description": "d2",
		"base_version": "1.0.0",
		"files": []map[string]any{
			{"path": "deploy.yaml", "content": deploy},
		},
	}
	if rec := do(t, h, "PUT", "/api/apps/1/spec", save, &token); rec.Code != http.StatusOK {
		t.Fatalf("保存: %d %s", rec.Code, rec.Body)
	}
	got2 := readChart("2.0.0")
	for _, want := range []string{"version: 2.0.0", "required:", "- app.port", "marker_dir: /var/lib/wdp2",
		"check_mode: true", "- db.password", "values_from: marker", "clears_marker: true"} {
		if !strings.Contains(got2, want) {
			t.Fatalf("生成的 chart.yaml 应保留底本元数据，缺 %q:\n%s", want, got2)
		}
	}
}

// ---- 上传互斥 / 请求体上限 / scope 越权 / 删除审计 / 排队关停 ----

// uploadChart 发起一次 chart 上传（multipart），返回响应记录器。
func uploadChart(t *testing.T, h http.Handler, token, path, fileName string, tgz []byte) *httptest.ResponseRecorder {
	t.Helper()
	body := &bytes.Buffer{}
	mw := newMultipartWriter(body, nil, "tgz", fileName, tgz)
	req := httptest.NewRequest("POST", path, body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	addCookie(req, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// loginAs 以指定账号登录取会话 cookie。
func loginAs(t *testing.T, h http.Handler, user, password string) string {
	t.Helper()
	rec := do(t, h, "POST", "/api/login", map[string]string{"user": user, "password": password}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s 应 200: %d %s", user, rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	t.Fatalf("login %s 无会话 cookie", user)
	return ""
}

// chartTgzWithPad 构造含不可压缩填充文件的 chart 包（gzip 压不掉随机
// 字节，包体必然大于填充本身，用于超限上传测试）。
func chartTgzWithPad(t *testing.T, name, version string, pad []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(n string, b []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(b))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	write("chart.yaml", []byte(fmt.Sprintf("name: %s\nversion: %s\ndescription: pad\nrequired: []\n", name, version)))
	write("deploy.yaml", []byte("- name: e2e\n  hosts: all\n  tasks:\n    - name: say\n      shell: 'true'\n"))
	write("values.yaml", []byte("{}"))
	write("pad.bin", pad)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestConcurrentSameVersionUpload 并发同版本上传：恰一方成功；最终制品
// 内容与 DB 行一致；失败方不得删除已入库版本正引用的制品文件。
// 兼测"DB 行丢失但制品残留"场景：残留文件按版本已存在拒绝，不得覆盖。
func TestConcurrentSameVersionUpload(t *testing.T) {
	s, st := newAppServer(t, 18781)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	const N = 4
	markers := make([]string, N)
	bodies := make([]*bytes.Buffer, N)
	cts := make([]string, N)
	for i := range markers {
		markers[i] = fmt.Sprintf("racer-%d", i)
		bodies[i] = &bytes.Buffer{}
		mw := newMultipartWriter(bodies[i], nil, "tgz", "race.tgz", buildChartTgz(t, "race", "1.0.0", markers[i]))
		cts[i] = mw.FormDataContentType()
	}
	codes := make([]int, N)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest("POST", "/api/apps/upload", bodies[i])
			req.Header.Set("Content-Type", cts[i])
			addCookie(req, token)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	close(start)
	wg.Wait()

	created, rejected, winner := 0, 0, -1
	for i, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
			winner = i
		case http.StatusBadRequest:
			rejected++
		default:
			t.Fatalf("非预期状态码 %d（第 %d 个请求）", c, i)
		}
	}
	if created != 1 || rejected != N-1 {
		t.Fatalf("应恰 1 成功 %d 拒绝: %v", N-1, codes)
	}
	apps, _ := st.ListApps()
	if len(apps) != 1 || apps[0].VersionCount != 1 {
		t.Fatalf("应用应恰 1 行 1 版本: %+v", apps)
	}
	tgz, err := st.VersionTgz(apps[0].ID, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tgz); err != nil {
		t.Fatalf("制品文件不得被并发方删除: %v", err)
	}
	spec, err := readChartSpecInTest(tgz)
	if err != nil {
		t.Fatalf("制品应仍可加载: %v", err)
	}
	if !strings.Contains(spec.DeployYAML, markers[winner]) {
		t.Fatalf("制品内容应与成功入库的一方一致（期望 %s）:\n%s", markers[winner], spec.DeployYAML)
	}

	// DB 行丢失但制品文件残留（版本行不在、文件遗留）→ 按版本已存在拒绝，
	// rename 不得覆盖残留文件
	residue := filepath.Join(s.opts.DataDir, "apps", "leftover", "0.9.0.tgz")
	if err := os.MkdirAll(filepath.Dir(residue), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(residue, []byte("residue-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := uploadChart(t, h, token, "/api/apps/upload", "leftover.tgz", buildChartTgz(t, "leftover", "0.9.0", "x"))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "不可覆盖") {
		t.Fatalf("制品残留应按版本已存在拒绝: %d %s", rec.Code, rec.Body)
	}
	b, _ := os.ReadFile(residue)
	if string(b) != "residue-content" {
		t.Fatalf("残留制品不得被覆盖: %q", b)
	}
}

// TestUploadSizeLimit 上传请求体超限 → 413（无总量上限时可被无限灌盘）。
// 两个上传端点都验证。
func TestUploadSizeLimit(t *testing.T) {
	s, st := newAppServer(t, 18782)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	// 先以正常上限建应用（供第二个端点用）
	rec := uploadChart(t, h, token, "/api/apps/upload", "big.tgz", buildChartTgz(t, "big", "1.0.0", "x"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("建应用应 201: %d %s", rec.Code, rec.Body)
	}
	var app store.App
	json.Unmarshal(rec.Body.Bytes(), &app)

	// 收窄总量上限（maxUploadBytes 为测试可调变量）；填充不可压缩，
	// 包体必然超过上限
	old := maxUploadBytes
	maxUploadBytes = 8192
	defer func() { maxUploadBytes = old }()
	rnd := rand.New(rand.NewSource(1))
	pad := make([]byte, 64*1024)
	rnd.Read(pad)
	bigTgz := chartTgzWithPad(t, "big", "2.0.0", pad)

	if rec := uploadChart(t, h, token, "/api/apps/upload", "big.tgz", bigTgz); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限上传应 413: %d %s", rec.Code, rec.Body)
	}
	if rec := uploadChart(t, h, token, fmt.Sprintf("/api/apps/%d/versions/upload", app.ID), "big.tgz", bigTgz); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限加版本应 413: %d %s", rec.Code, rec.Body)
	}
	a, _ := st.GetApp(app.ID)
	if a.VersionCount != 1 {
		t.Fatalf("超限上传不得入库: %+v", a)
	}
}

// TestScopedEditCannotEscalateScope 作用域级 app:edit/app:upload 用户不得
// 把应用/版本 scope 写到未授权池/组/标签（扩 scope 是 app:scope 的权限
// 语义）；授权范围内正常成功；admin 不受限。
func TestScopedEditCannotEscalateScope(t *testing.T) {
	s, st := newAppServer(t, 18783)
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")

	rec := do(t, h, "POST", "/api/apps/spec", map[string]any{
		"name": "scoped-app", "version": "1.0.0", "description": "",
		"files": []map[string]any{
			{"path": "chart.yaml", "content": "name: scoped-app\nversion: 1.0.0\n"},
			{"path": "values.yaml", "content": "a: 1\n"},
			{"path": "deploy.yaml", "content": "- name: s\n  hosts: all\n  tasks:\n    - name: t\n      shell: 'true'\n"},
		},
		"pools": []string{"pool-a"},
	}, &admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("建应用应 201: %d %s", rec.Code, rec.Body)
	}
	var app store.App
	json.Unmarshal(rec.Body.Bytes(), &app)

	// scoped 用户：viewer + app:edit@pool-a + app:upload@pool-a
	hash, err := bcrypt.GenerateFromPassword([]byte("scoped-pass-1"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser("scoped", string(hash), "viewer"); err != nil {
		t.Fatal(err)
	}
	u, err := st.UserByName("scoped")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceUserScopes(u.ID, []*store.UserScope{
		{Verb: verbAppEdit, Kind: "pool", Value: "pool-a"},
		{Verb: verbAppUpload, Kind: "pool", Value: "pool-a"},
	}); err != nil {
		t.Fatal(err)
	}
	s.invalidatePerms("scoped")
	scoped := loginAs(t, h, "scoped", "scoped-pass-1")

	deploy := "- name: s\n  hosts: all\n  tasks:\n    - name: t\n      shell: 'true'\n"
	save := func(version string, pools, groups []string, labels string) *httptest.ResponseRecorder {
		return do(t, h, "PUT", fmt.Sprintf("/api/apps/%d/spec", app.ID), map[string]any{
			"version": version, "description": "",
			"files": []map[string]any{
				{"path": "chart.yaml", "content": "name: scoped-app\nversion: " + version + "\n"},
				{"path": "values.yaml", "content": "a: 1\n"},
				{"path": "deploy.yaml", "content": deploy},
			},
			"base_version": "1.0.0",
			"pools":        pools, "groups": groups, "labels": labels,
		}, &scoped)
	}

	// 1. 编辑器保存把 scope 扩到未授权池 → 403 且信息指明 pool 越权
	if rec := save("2.0.0", []string{"pool-z"}, nil, ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "pool") {
		t.Fatalf("扩到未授权池应 403 且指明 pool: %d %s", rec.Code, rec.Body)
	}
	if a, _ := st.GetApp(app.ID); a.VersionCount != 1 {
		t.Fatalf("拒绝后不得入库: %+v", a)
	}
	// 2. 未授权标签 → 403 且指明 label
	if rec := save("2.0.0", []string{"pool-a"}, nil, `{"env":"prod"}`); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "label") {
		t.Fatalf("未授权标签应 403 且指明 label: %d %s", rec.Code, rec.Body)
	}
	// 3. 授权池内保存 → 成功
	if rec := save("2.0.0", []string{"pool-a"}, nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("授权池内保存应 200: %d %s", rec.Code, rec.Body)
	}
	// 4. 应用元数据（PUT /api/apps/{id}）扩到未授权组 → 403 且指明 group
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/apps/%d", app.ID), map[string]any{"note": "n", "groups": []string{"grp-z"}}, &scoped); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "group") {
		t.Fatalf("扩到未授权组应 403 且指明 group: %d %s", rec.Code, rec.Body)
	}
	// 5. 加版本继承的 scope 越权：admin 把应用扩到 pool-a+pool-b 后，
	//    scoped 上传新版本将把 pool-b 写进版本行 → 403
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/apps/%d", app.ID), map[string]any{"note": "n", "pools": []string{"pool-a", "pool-b"}}, &admin); rec.Code != http.StatusOK {
		t.Fatalf("admin 扩 scope 应 200: %d %s", rec.Code, rec.Body)
	}
	if rec := uploadChart(t, h, scoped, fmt.Sprintf("/api/apps/%d/versions/upload", app.ID), "scoped-app.tgz", buildChartTgz(t, "scoped-app", "3.0.0", "x")); rec.Code != http.StatusForbidden {
		t.Fatalf("继承越权 scope 的上传应 403: %d %s", rec.Code, rec.Body)
	}
	// 6. admin 同样操作不受限
	if rec := uploadChart(t, h, admin, fmt.Sprintf("/api/apps/%d/versions/upload", app.ID), "scoped-app.tgz", buildChartTgz(t, "scoped-app", "3.0.0", "x")); rec.Code != http.StatusCreated {
		t.Fatalf("admin 上传应 201: %d %s", rec.Code, rec.Body)
	}
}

// TestDeleteAppAuditKeepsName 删除应用的审计须保留应用名（删后取名必为
// ErrNotFound，审计恒丢名）。
func TestDeleteAppAuditKeepsName(t *testing.T) {
	s, st := newAppServer(t, 18784)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	rec := uploadChart(t, h, token, "/api/apps/upload", "gone.tgz", buildChartTgz(t, "gone", "1.0.0", "x"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传应 201: %d %s", rec.Code, rec.Body)
	}
	var app store.App
	json.Unmarshal(rec.Body.Bytes(), &app)
	if rec := do(t, h, "DELETE", fmt.Sprintf("/api/apps/%d", app.ID), nil, &token); rec.Code != http.StatusOK {
		t.Fatalf("删除应 200: %d %s", rec.Code, rec.Body)
	}
	logs, err := st.ListAuditLogs(50, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range logs {
		if l.Action == "delete" && l.Object == "app" {
			found = true
			if l.Name != "gone" {
				t.Fatalf("删除审计应保留应用名: %+v", l)
			}
		}
	}
	if !found {
		t.Fatal("缺少应用删除审计记录")
	}
}

// TestRunQueueAbortOnShutdown 排队中的 run 在 server 生命周期 ctx 取消
// （关停）时置 failed 并退出，不得无限等待主机闸门。
func TestRunQueueAbortOnShutdown(t *testing.T) {
	s, st := newAppServer(t, 18785)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	rec := uploadChart(t, h, token, "/api/apps/upload", "abort.tgz", buildChartTgz(t, "abort", "1.0.0", "x"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传应 201: %d %s", rec.Code, rec.Body)
	}
	var app store.App
	json.Unmarshal(rec.Body.Bytes(), &app)

	// 占住主机闸门：run 只能排队等锁
	release := s.gate.Acquire([]int64{1})
	// 注入可取消的生命周期 ctx（等价 server.Run 注入的 bgCtx）
	bg, cancel := context.WithCancel(context.Background())
	s.bgCtx = bg

	rec = do(t, h, "POST", "/api/runs", map[string]any{
		"items":    []map[string]any{{"app_id": app.ID}},
		"selector": map[string]any{"kind": "all"},
	}, &token)
	if rec.Code != http.StatusAccepted {
		release()
		t.Fatalf("执行应 202: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		RunIDs []int64 `json:"run_ids"`
	}
	json.Unmarshal(rec.Body.Bytes(), &started)

	// 取消生命周期 ctx：排队中的 run 应被置 failed 而非无限等待
	time.Sleep(200 * time.Millisecond)
	cancel()

	var run *store.Run
	for i := 0; i < 100; i++ {
		run, _ = st.GetRun(started.RunIDs[0])
		if run != nil && run.Status == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	release()
	if run == nil || run.Status != "failed" || !strings.Contains(run.Summary, "关停") {
		t.Fatalf("关停应中断排队并置 failed: %+v", run)
	}
}

// TestWebRunBareTasksAndGroupedPlays 新 hosts 语义 e2e：
//  1. 裸任务相位（无 play 包装、无 hosts）→ web 打选择器全集，任务正常执行；
//  2. 显式 hosts 的 play 在选择器范围内按台账组细分——组含选择器主机
//     的 play 执行，组不含的 play 空跑跳过。
func TestWebRunBareTasksAndGroupedPlays(t *testing.T) {
	s, st := newAppServer(t, 18769)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	up := func(name, version, deploy string) int64 {
		t.Helper()
		body := &bytes.Buffer{}
		mw := newMultipartWriter(body, nil, "tgz", name+".tgz",
			buildChartTgzFull(t, name, version, "", deploy, "{}"))
		req := httptest.NewRequest("POST", "/api/apps/upload", body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		addCookie(req, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("上传 %s 应 201: %d %s", name, rec.Code, rec.Body)
		}
		var app store.App
		json.Unmarshal(rec.Body.Bytes(), &app)
		return app.ID
	}
	run := func(appID int64) (*store.Run, []string) {
		t.Helper()
		rbody := `{"items":[{"app_id":` + fmt.Sprint(appID) + `}],"selector":{"kind":"all"}}`
		req := httptest.NewRequest("POST", "/api/runs", strings.NewReader(rbody))
		req.Header.Set("Content-Type", "application/json")
		addCookie(req, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("执行应 202: %d %s", rec.Code, rec.Body)
		}
		var started struct {
			RunIDs []int64 `json:"run_ids"`
		}
		json.Unmarshal(rec.Body.Bytes(), &started)
		var run *store.Run
		for i := 0; i < 100; i++ {
			run, _ = st.GetRun(started.RunIDs[0])
			if run != nil && run.Status != "running" && run.Status != "queued" {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		ts, _ := st.RunTasks(started.RunIDs[0])
		msgs := []string{}
		for _, x := range ts {
			msgs = append(msgs, x.Play+"/"+x.Task+"@"+x.Host+":"+x.Status)
		}
		return run, msgs
	}

	// 1) 裸任务相位（直接任务列表，无 hosts）
	bareID := up("bare-app", "1.0.0", "- name: 裸任务一\n  shell: 'echo bare-ok'\n- name: 裸任务二\n  shell: 'echo bare-2'\n")
	run1, msgs1 := run(bareID)
	if run1 == nil || run1.Status != "succeeded" {
		t.Fatalf("裸任务相位 run 应成功: %+v %v", run1, msgs1)
	}
	joined := strings.Join(msgs1, " ")
	if !strings.Contains(joined, "裸任务一@e2e-local") || !strings.Contains(joined, "裸任务二@e2e-local") {
		t.Fatalf("裸任务应在选择器主机上执行: %v", msgs1)
	}

	// 2) 分组 play：台账建 grp-hit（含 e2e-local）与 grp-miss（空）
	if _, err := st.CreateGroup("grp-hit", "", []int64{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateGroup("grp-miss", "", nil); err != nil {
		t.Fatal(err)
	}
	grouped := `- name: 命中组
  hosts: grp-hit
  tasks:
    - name: 打标记
      shell: 'echo grp-hit-ran'
- name: 未命中组
  hosts: grp-miss
  tasks:
    - name: 不该执行
      shell: 'echo grp-miss-ran'
`
	groupedID := up("grouped-app", "1.0.0", grouped)
	run2, msgs2 := run(groupedID)
	if run2 == nil || run2.Status != "succeeded" {
		t.Fatalf("分组 play run 应成功: %+v %v", run2, msgs2)
	}
	joined2 := strings.Join(msgs2, " ")
	if !strings.Contains(joined2, "命中组/打标记@e2e-local") {
		t.Fatalf("命中组的 play 应执行: %v", msgs2)
	}
	if strings.Contains(joined2, "不该执行") {
		t.Fatalf("未命中组的 play 不应执行: %v", msgs2)
	}

	// 3) 旧 chart 兼容：hosts 写不存在的组名 → 回退全集执行（非空跑）
	legacyID := up("legacy-app", "1.0.0", "- name: p\n  hosts: legacy-app\n  tasks:\n    - name: 打标记\n      shell: 'echo legacy-ran'\n")
	run3, msgs3 := run(legacyID)
	if run3 == nil || run3.Status != "succeeded" {
		t.Fatalf("旧写法 run 应成功: %+v %v", run3, msgs3)
	}
	if !strings.Contains(strings.Join(msgs3, " "), "@e2e-local") {
		t.Fatalf("未知组名应回退全集执行: %v", msgs3)
	}
}
