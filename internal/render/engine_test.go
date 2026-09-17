package render

import (
	"strings"
	"testing"
)

func TestEngineNamedTemplate(t *testing.T) {
	e, err := NewEngine(`{{ define "app.name" }}{{ .name }}-{{ .env }}{{ end }}`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.Render(`svc: {{ template "app.name" . }}`, map[string]any{"name": "web", "env": "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "svc: web-prod" {
		t.Fatalf("got %q", out)
	}
}

func TestEngineIncludeWithPipe(t *testing.T) {
	e, err := NewEngine(`{{ define "svc.port" }}{{ .port }}{{ end }}`)
	if err != nil {
		t.Fatal(err)
	}
	// include 返回字符串，可参与管道
	out, err := e.Render(`PORT={{ include "svc.port" . | upper }}`, map[string]any{"port": "8080"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "PORT=8080" {
		t.Fatalf("got %q", out)
	}
}

// TestEngineRecursiveIncludeErrors 自引用/互引用的 helper 必须返回错误，
// 而不是把进程吃到栈溢出（Go 的 maxExecDepth 只保护 {{template}} 动作，
// 普通函数递归不受保护，栈溢出是 fatal error，不可 recover）。
func TestEngineRecursiveIncludeErrors(t *testing.T) {
	cases := map[string]string{
		"self": `{{ define "loop" }}{{ include "loop" . }}{{ end }}`,
		"mutual": `{{ define "a" }}{{ include "b" . }}{{ end }}` +
			`{{ define "b" }}{{ include "a" . }}{{ end }}`,
	}
	for name, helpers := range cases {
		t.Run(name, func(t *testing.T) {
			e, err := NewEngine(helpers)
			if err != nil {
				t.Fatal(err)
			}
			entry := "loop"
			if name == "mutual" {
				entry = "a"
			}
			if _, err := e.Render(`{{ include "`+entry+`" . }}`, map[string]any{}); err == nil ||
				!strings.Contains(err.Error(), "include depth exceeded") {
				t.Fatalf("递归 include 应报深度超限: %v", err)
			}
		})
	}
}

// TestEngineNestedIncludeStillWorks 深度计数不能误伤合法嵌套。
func TestEngineNestedIncludeStillWorks(t *testing.T) {
	e, err := NewEngine(`{{ define "outer" }}[{{ include "inner" . }}]{{ end }}` +
		`{{ define "inner" }}{{ .v }}{{ end }}`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.Render(`{{ include "outer" . }}`, map[string]any{"v": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "[x]" {
		t.Fatalf("got %q", out)
	}
}

func TestEngineHelpersAcrossValues(t *testing.T) {
	e, _ := NewEngine(`{{ define "fqdn" }}{{ .host }}.{{ .domain }}{{ end }}`)
	// RenderValue（map/slice 递归）同样能引用 helpers
	v, err := e.RenderValue(map[string]any{
		"endpoint": "{{ template \"fqdn\" . }}",
		"list":     []any{"{{ template \"fqdn\" . }}"},
	}, map[string]any{"host": "web1", "domain": "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if m["endpoint"] != "web1.example.com" {
		t.Fatalf("%v", m)
	}
	if m["list"].([]any)[0] != "web1.example.com" {
		t.Fatalf("%v", m)
	}
}

func TestEngineIsolation(t *testing.T) {
	_, err := NewEngine(`{{ define "a" }}one{{ end }}`)
	if err != nil {
		t.Fatal(err)
	}
	e2, _ := NewEngine(``)
	// e1 的命名模板不影响默认引擎 / e2
	if _, err := e2.Render(`{{ template "a" . }}`, nil); err == nil {
		t.Fatal("e2 不应看到 e1 的命名模板")
	}
	if _, err := defaultEngine.Render(`{{ template "a" . }}`, nil); err == nil {
		t.Fatal("默认引擎不应看到 e1 的命名模板")
	}
	// helpers 解析错误
	if _, err := NewEngine(`{{ define "bad" }}{{ end`); err == nil {
		t.Fatal("坏模板应报错")
	}
}

func TestEngineMissingKey(t *testing.T) {
	e, _ := NewEngine("")
	if _, err := e.Render(`{{ .nope }}`, map[string]any{}); err == nil {
		t.Fatal("未定义变量应报错")
	}
}
