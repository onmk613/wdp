package web

// IDE 支撑端点测试：整体校验（validate）、草稿 CRUD（draft）、
// schema 元数据、spec 的 chart.yaml verbatim 往返（注释保真 + 一致性对账）。
// validate 请求体是 {app_id, name, …specReq 字段平铺}（specReq 匿名内嵌，
// JSON 字段提升）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wdp/internal/store"
)

// doJSON 发 JSON 请求并解出响应体。
func doJSON(t *testing.T, h http.Handler, method, path string, body any, cookie *string, out any) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(t, h, method, path, body, cookie)
	if out != nil {
		json.Unmarshal(rec.Body.Bytes(), out)
	}
	return rec
}

type issuesResp struct {
	Issues []validateIssue `json:"issues"`
}

// deref 展开 SpecFile.Content（nil = 未提交，测试里按空串处理）。
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ideSpecBody 构造 IDE 形态的保存/校验请求体（三件套以 verbatim 文件提交）。
func ideSpecBody(name, version, chartYAML, valuesYAML, deployYAML string) map[string]any {
	return map[string]any{
		"name":         name,
		"version":      version,
		"description":  "",
		"base_version": "",
		"pools":        []string{},
		"groups":       []string{},
		"labels":       "{}",
		"files": []map[string]any{
			{"path": "chart.yaml", "content": chartYAML},
			{"path": "values.yaml", "content": valuesYAML},
			{"path": "deploy.yaml", "content": deployYAML},
		},
	}
}

const ideDeployOK = "- name: deploy\n  hosts: all\n  tasks:\n    - shell: 'echo hi'\n"
const ideDeployBadModule = "- name: deploy\n  hosts: all\n  tasks:\n    - nosuchmod: 'x'\n"

// TestValidateSpec validate 端点（新建模式）：干净 chart 无 ERROR；未知模块
// 出 ERROR；物化失败（deploy 为空）也归为一条 ERROR（统一问题面板口径）。
func TestValidateSpec(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	// 正常 chart（三件套 verbatim）
	var resp issuesResp
	rec := doJSON(t, h, "POST", "/api/apps/validate", ideSpecBody("vapp", "1.0.0",
		"name: vapp\nversion: 1.0.0\n", "{}\n", ideDeployOK), &token, &resp)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate 应 200: %d %s", rec.Code, rec.Body)
	}
	for _, is := range resp.Issues {
		if is.Level == "ERROR" {
			t.Fatalf("干净 chart 不应有 ERROR: %+v", resp.Issues)
		}
	}

	// 未知模块 → lint ERROR
	resp = issuesResp{}
	doJSON(t, h, "POST", "/api/apps/validate", ideSpecBody("vapp", "1.0.1",
		"name: vapp\nversion: 1.0.1\n", "{}\n", ideDeployBadModule), &token, &resp)
	found := false
	for _, is := range resp.Issues {
		if is.Level == "ERROR" && strings.Contains(is.Msg, "unknown module") {
			found = true
		}
	}
	if !found {
		t.Fatalf("未知模块应出 ERROR: %+v", resp.Issues)
	}

	// deploy 为空 → 物化失败归为一条 ERROR（而非 400/500）
	resp = issuesResp{}
	rec = doJSON(t, h, "POST", "/api/apps/validate", ideSpecBody("vapp", "1.0.2",
		"name: vapp\nversion: 1.0.2\n", "{}\n", "   \n"), &token, &resp)
	if rec.Code != http.StatusOK || len(resp.Issues) != 1 || resp.Issues[0].Level != "ERROR" {
		t.Fatalf("物化失败应为 200 + 单条 ERROR: %d %s", rec.Code, rec.Body)
	}
}

// TestValidateChartVersionReconciled 校验与保存同口径：基于已有版本修改、
// chart.yaml 还带着已被占用的底本版本号时，校验按"保存将产生的形态"
// 虚拟改写版本——不得报版本对账 ERROR（保存时前端会 patch，对账只在
// 保存路径是硬约束）。
func TestValidateChartVersionReconciled(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	// 请求版本 1.0.3（下一个可用），chart.yaml 仍写底本 1.0.2
	var resp issuesResp
	rec := doJSON(t, h, "POST", "/api/apps/validate", ideSpecBody("vapp2", "1.0.3",
		"# 保留注释\nname: vapp2\nversion: 1.0.2\n", "{}\n", ideDeployOK), &token, &resp)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate 应 200: %d %s", rec.Code, rec.Body)
	}
	for _, is := range resp.Issues {
		if strings.Contains(is.Msg, "does not match requested version") {
			t.Fatalf("校验不应报版本对账 ERROR（保存时会自动 patch）: %+v", resp.Issues)
		}
		if is.Level == "ERROR" {
			t.Fatalf("干净 chart 不应有 ERROR: %+v", resp.Issues)
		}
	}
}

// TestValidateSpecEditMode 编辑模式：app_id>0 时以底本为基底（未提交的
// 文件原样参与校验）。
func TestValidateSpecEditMode(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	// 建应用（spec 创建，三件套 verbatim）
	var app store.App
	rec := doJSON(t, h, "POST", "/api/apps/spec", ideSpecBody("eapp", "2.0.0",
		"# top comment\nname: eapp\nversion: 2.0.0\n", "{}\n", ideDeployOK), &token, &app)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建应 201: %d %s", rec.Code, rec.Body)
	}

	// 校验：仅提交改过的 deploy（其余走底本），app 名以库为准
	part := map[string]any{
		"app_id":       app.ID,
		"version":      "2.0.1",
		"base_version": "2.0.0",
		"files": []map[string]any{
			{"path": "deploy.yaml", "content": ideDeployBadModule},
		},
	}
	var resp issuesResp
	doJSON(t, h, "POST", "/api/apps/validate", part, &token, &resp)
	found := false
	for _, is := range resp.Issues {
		if is.Level == "ERROR" && strings.Contains(is.Msg, "unknown module") {
			found = true
		}
	}
	if !found {
		t.Fatalf("编辑模式应检出未知模块: %+v", resp.Issues)
	}
}

// TestSpecChartYAMLVerbatim 三件套 verbatim 落盘：chart.yaml 注释逐字
// 保留；name/version 与请求不一致时拒绝（防库版本号与包内说法漂移）。
func TestSpecChartYAMLVerbatim(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	chartYAML := "# 顶部注释不该丢\nname: capp # 行尾注释\nversion: 1.0.0\nrequired: [app.port]\n"
	var app store.App
	rec := doJSON(t, h, "POST", "/api/apps/spec", ideSpecBody("capp", "1.0.0",
		chartYAML, "app:\n  port: 8080\n", ideDeployOK), &token, &app)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建应 201: %d %s", rec.Code, rec.Body)
	}

	// 读回 spec：chart.yaml 原文进 files（注释在）
	var spec AppSpec
	doJSON(t, h, "GET", "/api/apps/1/spec", nil, &token, &spec)
	got := ""
	for _, f := range spec.Files {
		if f.Path == "chart.yaml" {
			got = deref(f.Content)
		}
	}
	if !strings.Contains(got, "# 顶部注释不该丢") || !strings.Contains(got, "# 行尾注释") {
		t.Fatalf("chart.yaml 原文（含注释）应原样返回: %q", got)
	}
	if !strings.Contains(got, "required: [app.port]") {
		t.Fatalf("chart.yaml 其余字段应保留: %q", got)
	}

	// 不一致：请求版本 ≠ chart.yaml 版本 → 400
	bad := ideSpecBody("capp", "9.9.9", chartYAML, "{}\n", ideDeployOK)
	rec = doJSON(t, h, "POST", "/api/apps/spec", bad, &token, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "does not match") {
		t.Fatalf("版本不一致应 400: %d %s", rec.Code, rec.Body)
	}
}

// TestDraftCRUD 草稿写读删 + 上限 + 键校验。
func TestDraftCRUD(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	put := func(key, payload string) *httptest.ResponseRecorder {
		return do(t, h, "PUT", "/api/apps/draft?key="+key,
			map[string]string{"base_version": "1.0.0", "payload": payload}, &token)
	}
	// 写（新建键）
	if rec := put("new:dapp", `{"files":{}}`); rec.Code != http.StatusOK {
		t.Fatalf("草稿写入应 200: %d %s", rec.Code, rec.Body)
	}
	// 读回
	var got struct {
		Payload     string `json:"payload"`
		BaseVersion string `json:"base_version"`
		UpdatedAt   string `json:"updated_at"`
	}
	if rec := doJSON(t, h, "GET", "/api/apps/draft?key=new:dapp", nil, &token, &got); rec.Code != http.StatusOK {
		t.Fatalf("草稿读取应 200: %d %s", rec.Code, rec.Body)
	}
	if got.Payload != `{"files":{}}` || got.BaseVersion != "1.0.0" || got.UpdatedAt == "" {
		t.Fatalf("草稿内容异常: %+v", got)
	}
	// 覆盖写
	put("new:dapp", `{"files":{"a":1}}`)
	doJSON(t, h, "GET", "/api/apps/draft?key=new:dapp", nil, &token, &got)
	if got.Payload != `{"files":{"a":1}}` {
		t.Fatalf("同键覆盖应生效: %+v", got)
	}
	// 删除 → 404
	if rec := do(t, h, "DELETE", "/api/apps/draft?key=new:dapp", nil, &token); rec.Code != http.StatusOK {
		t.Fatalf("删除应 200: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/api/apps/draft?key=new:dapp", nil, &token); rec.Code != http.StatusNotFound {
		t.Fatalf("删除后应 404: %d", rec.Code)
	}
	// 非法键
	if rec := do(t, h, "GET", "/api/apps/draft?key=oops", nil, &token); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法键应 400: %d", rec.Code)
	}
	// 超限
	if rec := put("new:dapp", strings.Repeat("x", draftPayloadLimit+1)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限应 413: %d", rec.Code)
	}
}

// TestDraftsList 草稿箱列表：多键列出、应用名解析、孤儿草稿标记与可删。
func TestDraftsList(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	// 底本应用（编辑键用）+ 两份新建草稿
	app := ideSpecBody("lapp", "1.0.0", "name: lapp\nversion: 1.0.0\n", "{}\n", ideDeployOK)
	var created store.App
	if rec := doJSON(t, h, "POST", "/api/apps/spec", app, &token, &created); rec.Code != http.StatusCreated {
		t.Fatalf("建应用应 201: %d %s", rec.Code, rec.Body)
	}
	put := func(key string) {
		if rec := do(t, h, "PUT", "/api/apps/draft?key="+key,
			map[string]string{"base_version": "", "payload": "{}"}, &token); rec.Code != http.StatusOK {
			t.Fatalf("草稿写入应 200 (%s): %d %s", key, rec.Code, rec.Body)
		}
	}
	put(fmt.Sprintf("%d", created.ID))
	put("new:wip1")

	// 孤儿草稿：先对存在的应用存草稿，再删应用（PUT 对不存在的应用
	// 本就拒绝——孤儿只能事后产生）
	var gone store.App
	if rec := doJSON(t, h, "POST", "/api/apps/spec",
		ideSpecBody("goner", "1.0.0", "name: goner\nversion: 1.0.0\n", "{}\n", ideDeployOK), &token, &gone); rec.Code != http.StatusCreated {
		t.Fatalf("建应用应 201: %d %s", rec.Code, rec.Body)
	}
	put(fmt.Sprintf("%d", gone.ID))
	if _, err := s.st.DeleteApp(gone.ID); err != nil {
		t.Fatal(err)
	}

	var list []struct {
		Key     string `json:"key"`
		Kind    string `json:"kind"`
		AppName string `json:"app_name"`
		AppGone bool   `json:"app_gone"`
	}
	if rec := doJSON(t, h, "GET", "/api/apps/drafts", nil, &token, &list); rec.Code != http.StatusOK {
		t.Fatalf("列表应 200: %d %s", rec.Code, rec.Body)
	}
	byKey := map[string]int{}
	for i, it := range list {
		byKey[it.Key] = i
	}
	if len(list) != 3 {
		t.Fatalf("应有 3 份草稿: %+v", list)
	}
	if i, ok := byKey[fmt.Sprintf("%d", created.ID)]; !ok || list[i].Kind != "edit" || list[i].AppName != "lapp" || list[i].AppGone {
		t.Fatalf("编辑草稿元数据异常: %+v", list)
	}
	if i, ok := byKey["new:wip1"]; !ok || list[i].Kind != "new" || list[i].AppName != "wip1" {
		t.Fatalf("新建草稿元数据异常: %+v", list)
	}
	if i, ok := byKey[fmt.Sprintf("%d", gone.ID)]; !ok || !list[i].AppGone {
		t.Fatalf("孤儿草稿应标记 app_gone: %+v", list)
	}
	// 孤儿草稿可删（应用不存在，权限对象消失）
	if rec := do(t, h, "DELETE", "/api/apps/draft?key="+fmt.Sprintf("%d", gone.ID), nil, &token); rec.Code != http.StatusOK {
		t.Fatalf("孤儿草稿删除应 200: %d %s", rec.Code, rec.Body)
	}
}

// TestPlaybookValidate 裸 playbook 校验端点：合法通过、未知模块/语法错误为 ERROR。
func TestPlaybookValidate(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)
	post := func(content string) []struct{ Level, Msg string } {
		var resp struct {
			Issues []struct{ Level, Msg string }
		}
		rec := doJSON(t, h, "POST", "/api/playbook/validate",
			map[string]string{"content": content}, &token, &resp)
		if rec.Code != http.StatusOK {
			t.Fatalf("校验应 200: %d %s", rec.Code, rec.Body)
		}
		return resp.Issues
	}
	if is := post("- name: ok\n  shell: echo hi\n"); len(is) != 0 {
		t.Fatalf("合法 playbook 应无发现: %+v", is)
	}
	is := post("- name: bad\n  nosuchmod: {}\n")
	if len(is) == 0 || is[0].Level != "ERROR" || !strings.Contains(is[0].Msg, "nosuchmod") {
		t.Fatalf("未知模块应为 ERROR: %+v", is)
	}
	is = post(":::not yaml\n@@@")
	if len(is) == 0 || is[0].Level != "ERROR" {
		t.Fatalf("语法错误应为 ERROR: %+v", is)
	}
}

// TestSchemaEndpoint /api/schema：字段表 + 内置变量 + 模板函数。
func TestSchemaEndpoint(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	var resp schemaResp
	if rec := doJSON(t, h, "GET", "/api/schema", nil, &token, &resp); rec.Code != http.StatusOK {
		t.Fatalf("schema 应 200: %d %s", rec.Code, rec.Body)
	}
	if len(resp.Task) == 0 || len(resp.Play) == 0 || len(resp.Chart) == 0 {
		t.Fatalf("字段分组表缺失: %+v", resp)
	}
	// 关键控制键有文档（前端补全依赖）
	hasKey := func(name string) bool {
		for _, sec := range resp.Task {
			for _, f := range sec.Fields {
				if f.Name == name {
					return true
				}
			}
		}
		return false
	}
	for _, k := range []string{"when", "loop", "loop_control", "register", "notify", "until", "become", "delegate_to"} {
		if !hasKey(k) {
			t.Errorf("任务控制键 %q 缺文档", k)
		}
	}
	// 内置变量与模板函数非空（{{}} 补全候选）
	if len(resp.BuiltinVars) == 0 || len(resp.TemplateFuncs) == 0 {
		t.Fatalf("内置变量/模板函数缺失: %+v", resp)
	}
}

// TestPartialSaveKeepsBaseFiles 部分保存语义：编辑模式只提交改动的
// deploy.yaml，底本的 values.yaml/chart.yaml（含注释）必须原样保留
// （曾把未提交的 values.yaml 重写成 {}、模板渲染全炸）。
func TestPartialSaveKeepsBaseFiles(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	// 建应用：values 带变量、chart.yaml 带注释
	var app store.App
	body := ideSpecBody("papp", "1.0.0",
		"# comment\nname: papp\nversion: 1.0.0\n",
		"app:\n  port: 8080\n", ideDeployOK)
	rec := doJSON(t, h, "POST", "/api/apps/spec", body, &token, &app)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建应 201: %d %s", rec.Code, rec.Body)
	}

	// 部分保存（IDE 真实形态）：提交改动的 deploy.yaml + 版本号已写进的
	// chart.yaml（verbatim），values.yaml 不提交
	part := map[string]any{
		"version":      "1.0.1",
		"base_version": "1.0.0",
		"files": []map[string]any{
			{"path": "deploy.yaml", "content": "- name: deploy\n  hosts: all\n  tasks:\n    - shell: 'echo v2'\n"},
			{"path": "chart.yaml", "content": "# comment\nname: papp\nversion: 1.0.1\n"},
		},
	}
	if rec := do(t, h, "PUT", "/api/apps/1/spec", part, &token); rec.Code != http.StatusOK {
		t.Fatalf("部分保存应 200: %d %s", rec.Code, rec.Body)
	}

	// 新版本：未提交的 values.yaml 保留底本、chart.yaml 注释随 verbatim
	// 保留、deploy.yaml 是新内容
	var spec AppSpec
	doJSON(t, h, "GET", "/api/apps/1/spec?version=1.0.1", nil, &token, &spec)
	if !strings.Contains(spec.ValuesYAML, "port: 8080") {
		t.Fatalf("未提交的 values.yaml 应保留底本: %q", spec.ValuesYAML)
	}
	for _, f := range spec.Files {
		if f.Path == "chart.yaml" && !strings.Contains(deref(f.Content), "# comment") {
			t.Fatalf("verbatim 提交的 chart.yaml 注释应保留: %q", deref(f.Content))
		}
	}
	if !strings.Contains(spec.DeployYAML, "echo v2") {
		t.Fatalf("提交的 deploy.yaml 应更新: %q", spec.DeployYAML)
	}
}
