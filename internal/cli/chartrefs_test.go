package cli

// 裸 playbook chart 引用预扫描回归：charts/ 目录加载、helpers 聚合引擎、
// 缺引用与坏 chart 的启动期报错。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/playbook"
	"wdp/internal/render"
)

func writeRefsLayout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("site.yaml", `
- hosts: all
  tasks:
    - chart: {name: app, values: {x: 1}}
    - shell: uptime
`)
	write("charts/app/chart.yaml", "name: app\nversion: 1.0.0\n")
	write("charts/app/values.yaml", "a: b\n")
	write("charts/app/_helpers.tpl", `{{- define "app.name" -}}app{{- end -}}`)
	write("charts/app/deploy.yaml", `
- hosts: all
  tasks:
    - shell: 'echo {{ include "app.name" . }}'
`)
	return dir
}

func TestPreloadChartRefs(t *testing.T) {
	dir := writeRefsLayout(t)
	plays, err := playbook.Load(filepath.Join(dir, "site.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	refs, eng, err := preloadChartRefsFake(dir, plays)
	if err != nil {
		t.Fatal(err)
	}
	if refs["app"] == nil || refs["app"].Meta.Name != "app" {
		t.Fatalf("app 未加载: %v", refs)
	}
	if eng == nil {
		t.Fatal("带 _helpers.tpl 的引用应构建聚合引擎")
	}
	// helpers 确实进了引擎（include 可用）
	if _, err := eng.Render(`{{ include "app.name" . }}`, map[string]any{}); err != nil {
		t.Fatalf("helpers 聚合失效: %v", err)
	}

	// 缺引用：启动期报错并指明解析根
	if err := os.WriteFile(filepath.Join(dir, "site.yaml"), []byte("- hosts: all\n  tasks:\n    - chart: {name: nosuch}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plays2, _ := playbook.Load(filepath.Join(dir, "site.yaml"))
	_, _, err = preloadChartRefsFake(dir, plays2)
	if err == nil || !strings.Contains(err.Error(), "charts/") {
		t.Fatalf("缺引用应报错并指明 charts/ 解析根: %v", err)
	}

	// 无引用的 playbook：refs/引擎均为 nil（默认引擎路径）
	if err := os.WriteFile(filepath.Join(dir, "site.yaml"), []byte("- hosts: all\n  tasks:\n    - shell: uptime\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plays3, _ := playbook.Load(filepath.Join(dir, "site.yaml"))
	refs3, eng3, err := preloadChartRefsFake(dir, plays3)
	if err != nil || refs3 != nil || eng3 != nil {
		t.Fatalf("无引用应零开销跳过: %v %v %v", refs3, eng3, err)
	}
}

// render.DefaultEngine 引用占位（防止 import 被裁剪的编译期锚点不需要——
// 本文件实际使用见上；保留 types 引用说明）。
var _ = render.DefaultEngine
