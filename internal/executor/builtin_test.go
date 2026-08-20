package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"wdp/internal/chart"
	"wdp/internal/connection"
	"wdp/internal/model"
)

// TestBuiltinVars 验证 play_hosts/play_batch/groups/hosts 注入与模板可用性。
func TestBuiltinVars(t *testing.T) {
	ex, rep := setupFeature(t, false, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		return connection.ExecResult{Code: 0, Stdout: "ran: " + req.Script + "\n"}, nil
	})
	plays := []*model.Play{{
		Hosts: "webservers",
		Tasks: []*model.Task{
			{Name: "展开内置变量", Module: "shell",
				FreeForm: `self={{ .inventory_hostname }} batch={{ len .play_batch }} total={{ len .play_hosts }} group={{ index .groups "webservers" }} addr={{ (index .hosts "h2").address }}`},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	scripts := joinExecScripts(allFakes())
	if !strings.Contains(scripts, "self=h1 batch=2 total=2") {
		t.Fatalf("内置变量渲染错误:\n执行记录:\n%s\n结果:\n%s", scripts, rep.joined())
	}
	if !strings.Contains(scripts, "group=[h1 h2]") {
		t.Fatalf("groups 渲染错误:\n%s", scripts)
	}
	if !strings.Contains(scripts, "addr=h2") {
		t.Fatalf("hosts 元信息渲染错误:\n%s", scripts)
	}
}

// TestBuiltinVarsImmutable 验证内置变量不可被 play vars 覆盖。
func TestBuiltinVarsImmutable(t *testing.T) {
	ex, _ := setupFeature(t, false, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		return connection.ExecResult{Code: 0, Stdout: "ran: " + req.Script + "\n"}, nil
	})
	plays := []*model.Play{{
		Hosts: "h1",
		Vars:  map[string]any{"play_hosts": "被覆盖的值"},
		Tasks: []*model.Task{
			{Name: "验证", Module: "shell", FreeForm: "n={{ len .play_hosts }}"},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatal("不应失败")
	}
	if !strings.Contains(joinExecScripts(allFakes()), "n=1") {
		t.Fatalf("play_hosts 被覆盖:\n%s", joinExecScripts(allFakes()))
	}
}

// TestMarkerWriteAndRemove 验证 deploy 写 marker、uninstall 清除。
func TestMarkerWriteAndRemove(t *testing.T) {
	ch := &chart.Chart{}
	ch.Meta.Name = "markerapp"
	ch.Meta.MarkerDir = "/tmp/wdp-marker-test"

	run := func(phase string) {
		_, rep := setupFeature(t, false, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
			return connection.ExecResult{Code: 0}, nil
		})
		_ = rep
		// 直接构造执行器（复用 setupFeature 注册的 fake 工厂与 fakes 记录）
		inv := parseTestInv(t)
		rep2 := &captureReporter{}
		ex := New(inv, connection.NewManager(), rep2, Options{
			Forks: 2, Chart: ch, Phase: phase, WdpVersion: "test",
			Values: map[string]any{},
		})
		plays := []*model.Play{{Hosts: "h1", Tasks: []*model.Task{
			{Module: "shell", FreeForm: "true"},
		}}}
		if ex.Run(context.Background(), plays) {
			t.Fatalf("phase=%s 不应失败", phase)
		}
	}

	run("deploy")
	fs := allFakes()
	if len(fs) == 0 {
		t.Fatal("无 fake")
	}
	markerPath := "/tmp/wdp-marker-test/markerapp/release.json"
	content, ok := fs[0].File(markerPath)
	if !ok {
		t.Fatalf("marker 未写入: %#v", fs[0].Files)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		t.Fatalf("marker 非法 JSON: %v", err)
	}
	if m["chart"] != "markerapp" || m["phase"] != "deploy" {
		t.Fatalf("marker 内容: %s", content)
	}

	run("uninstall")
	// uninstall 后 marker 目录被清除（rm -rf 脚本出现在执行记录）
	if !strings.Contains(joinExecScripts(allFakes()), "rm -rf -- '/tmp/wdp-marker-test/markerapp'") {
		t.Fatalf("uninstall 未清除 marker:\n%s", joinExecScripts(allFakes()))
	}
}
