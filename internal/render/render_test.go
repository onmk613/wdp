package render

import "testing"

func TestRenderBasic(t *testing.T) {
	vars := map[string]any{"name": "web", "port": 8080}
	got, err := Render("hello {{ .name }}:{{ .port }}", vars)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello web:8080" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderNoTemplate(t *testing.T) {
	got, err := Render("plain text", nil)
	if err != nil || got != "plain text" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestRenderMissingKey(t *testing.T) {
	if _, err := Render("{{ .nope }}", map[string]any{}); err == nil {
		t.Fatal("期望未定义变量报错")
	}
}

func TestRenderFuncs(t *testing.T) {
	vars := map[string]any{"empty": ""}
	cases := []struct{ tpl, want string }{
		{`{{ default "80" .empty }}`, "80"},
		{`{{ upper "abc" }}`, "ABC"},
		{`{{ lower "ABC" }}`, "abc"},
		{`{{ trim "  x  " }}`, "x"},
		{`{{ b64enc "hi" }}`, "aGk="},
		{`{{ b64dec "aGk=" }}`, "hi"},
		{`{{ join (split " " "a b c") "," }}`, "a,b,c"},
		{`{{ replace "a" "b" "aaa" }}`, "bbb"},
		{`{{ contains "ell" "hello" }}`, "true"},
		{`{{ quote "hi" }}`, `"hi"`},
	}
	for _, c := range cases {
		got, err := Render(c.tpl, vars)
		if err != nil {
			t.Fatalf("%s: %v", c.tpl, err)
		}
		if got != c.want {
			t.Fatalf("%s: got %q want %q", c.tpl, got, c.want)
		}
	}
}

func TestRenderValueRecursive(t *testing.T) {
	vars := map[string]any{"h": "example.com"}
	in := map[string]any{
		"host":   "{{ .h }}",
		"list":   []any{"{{ .h }}", 42, nil},
		"nested": map[string]any{"x": "http://{{ .h }}"},
	}
	out, err := RenderValue(in, vars)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["host"] != "example.com" {
		t.Fatalf("host: %v", m["host"])
	}
	l := m["list"].([]any)
	if l[0] != "example.com" || l[1] != 42 {
		t.Fatalf("list: %v", l)
	}
	if m["nested"].(map[string]any)["x"] != "http://example.com" {
		t.Fatalf("nested: %v", m["nested"])
	}
}

func TestTruthy(t *testing.T) {
	for _, s := range []string{"", "false", "False", "0", "no", "[]", "<nil>"} {
		if Truthy(s) {
			t.Fatalf("%q 应为假", s)
		}
	}
	for _, s := range []string{"true", "1", "yes", "ok", "web1"} {
		if !Truthy(s) {
			t.Fatalf("%q 应为真", s)
		}
	}
}
