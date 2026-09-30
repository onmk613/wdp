package web

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestModulesEndpoint 模块自描述：全部内置模块可列出，参数表非空
// （debug 除外），shell 带 free-form 标记，新增模块自动出现。
func TestModulesEndpoint(t *testing.T) {
	s, _ := newTestServer(t)
	token := loginSession(t, s)
	rec := do(t, s.Handler(), "GET", "/api/modules", nil, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200: %d %s", rec.Code, rec.Body)
	}
	var mods []moduleInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &mods); err != nil {
		t.Fatal(err)
	}
	if len(mods) < 20 {
		t.Fatalf("模块数过少: %d", len(mods))
	}
	byName := map[string]moduleInfo{}
	for _, m := range mods {
		byName[m.Name] = m
	}
	sh, ok := byName["shell"]
	if !ok {
		t.Fatal("缺少 shell 模块")
	}
	if !sh.FreeForm || len(sh.Params) < 3 || sh.Example == "" {
		t.Fatalf("shell 元数据异常: %+v", sh)
	}
	if cp, ok := byName["copy"]; !ok || cp.Rollback != 2 { // copy 全量可回滚
		t.Fatalf("copy 回滚能力异常: %+v", byName["copy"])
	}
	if st, ok := byName["setup"]; !ok || !st.ReadOnly {
		t.Fatalf("setup 应只读: %+v", byName["setup"])
	}
	// debug 补齐 Example() 后满足 UsageProvider（Params+Example），
	// var/msg 参数表进入端点与编辑器补全
	if dbg, ok := byName["debug"]; ok && len(dbg.Params) != 2 {
		t.Fatalf("debug 应带 var/msg 两个参数: %+v", dbg.Params)
	}
	// 枚举值域透出契约：编辑器值补全读 ParamDoc.enum（与解析白名单同源），
	// file.state 与 user.shell 是两类代表（硬校验值域 / 建议值）
	enumOf := func(m moduleInfo, key string) []string {
		for _, p := range m.Params {
			if p.Name == key {
				return p.Enum
			}
		}
		return nil
	}
	if e := enumOf(byName["file"], "state"); len(e) != 5 || e[0] != "file" {
		t.Fatalf("file.state 枚举异常: %v", e)
	}
	if e := enumOf(byName["user"], "shell"); len(e) != 3 || e[0] != "/sbin/nologin" {
		t.Fatalf("user.shell 建议值异常: %v", e)
	}
}
