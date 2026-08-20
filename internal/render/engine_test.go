package render

import "testing"

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
	if _, err := Render(`{{ template "a" . }}`, nil); err == nil {
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
