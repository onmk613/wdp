package chart

import (
	"reflect"
	"testing"
)

func TestMergeDeep(t *testing.T) {
	base := map[string]any{
		"app": map[string]any{
			"port": 8080,
			"name": "demo",
			"tags": []any{"a", "b"},
		},
		"debug": true,
		"drop":  "me",
	}
	override := map[string]any{
		"app": map[string]any{
			"port": 9090,
			"tags": []any{"x"}, // 列表整体替换
		},
		"debug": nil, // null 删除
		"drop":  nil,
	}
	got := Merge(base, override)

	want := map[string]any{
		"app": map[string]any{
			"port": 9090,
			"name": "demo",
			"tags": []any{"x"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	// 入参不被修改
	if base["app"].(map[string]any)["port"] != 8080 {
		t.Fatal("base 被修改")
	}
}

func TestMergeMapOverScalar(t *testing.T) {
	base := map[string]any{"a": "scalar"}
	override := map[string]any{"a": map[string]any{"b": 1}}
	got := Merge(base, override)
	if !reflect.DeepEqual(got["a"], map[string]any{"b": 1}) {
		t.Fatalf("标量应被 map 整体替换: %#v", got["a"])
	}
}

func TestSetPath(t *testing.T) {
	m := map[string]any{}
	out, err := SetPath(m, "a.b.c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if out["a"].(map[string]any)["b"].(map[string]any)["c"] != 1 {
		t.Fatalf("%#v", out)
	}

	// 已有路径深写入
	SetPath(m, "a.b.d", "x")
	if m["a"].(map[string]any)["b"].(map[string]any)["d"] != "x" {
		t.Fatal("深写入失败")
	}
}

// TestSetPathNullDeletes 回归：--set k=null 曾写入 nil 值而非删除键，
// 与 -f 覆盖文件的 null 删除语义及 README 承诺不一致。
func TestSetPathNullDeletes(t *testing.T) {
	m := map[string]any{
		"image": map[string]any{"repo": "nginx", "tag": "latest"},
		"keep":  "yes",
	}
	// 顶层键删除
	if _, err := SetPath(m, "keep", nil); err != nil {
		t.Fatal(err)
	}
	if _, exists := m["keep"]; exists {
		t.Fatal("--set k=null 应删除顶层键")
	}
	// 嵌套键删除
	if _, err := SetPath(m, "image.tag", nil); err != nil {
		t.Fatal(err)
	}
	if _, exists := m["image"].(map[string]any)["tag"]; exists {
		t.Fatal("--set image.tag=null 应删除嵌套键")
	}
	// 删除不存在的键应为无害 no-op
	if _, err := SetPath(m, "no.such.key", nil); err != nil {
		t.Fatalf("删除不存在的键不应报错: %v", err)
	}
}

// TestSetPathListNilKeepsPlaceholder 列表元素写 null 保持占位（不移动下标）。
func TestSetPathListNilKeepsPlaceholder(t *testing.T) {
	m := map[string]any{"hosts": []any{"h1", "h2"}}
	if _, err := SetPath(m, "hosts[1]", nil); err != nil {
		t.Fatal(err)
	}
	hosts := m["hosts"].([]any)
	if len(hosts) != 2 || hosts[0] != "h1" || hosts[1] != nil {
		t.Fatalf("列表元素置 null 应保持下标: %v", hosts)
	}
}

// TestBuildValuesNoAliasPollution 回归：BuildValues 曾浅拷贝默认 values，
// --set 的原地写会穿透到 c.Values 的嵌套 map 污染 chart 默认值。
func TestBuildValuesNoAliasPollution(t *testing.T) {
	c := &Chart{}
	c.Values = map[string]any{
		"image": map[string]any{"repo": "nginx", "tag": "1.25"},
	}
	if _, err := c.BuildValues(nil, []string{"image.tag=2.0"}); err != nil {
		t.Fatal(err)
	}
	if got := c.Values["image"].(map[string]any)["tag"]; got != "1.25" {
		t.Fatalf("chart 默认 values 被污染: tag=%v", got)
	}
}

// TestSubScopeIsolated 子 chart 作用域与子 chart 默认 values / 父 global 不共享引用。
func TestSubScopeIsolated(t *testing.T) {
	sub := &Chart{Meta: Meta{Name: "sub"}}
	sub.Values = map[string]any{"inner": map[string]any{"x": 1}}
	parent := map[string]any{"global": map[string]any{"g": 1}}
	scope := SubScope(sub, parent)
	scope["inner"].(map[string]any)["x"] = 999
	scope["global"].(map[string]any)["g"] = 999
	if sub.Values["inner"].(map[string]any)["x"] != 1 {
		t.Fatal("子 chart 默认 values 被作用域写入污染")
	}
	if parent["global"].(map[string]any)["g"] != 1 {
		t.Fatal("父 global 被子作用域写入污染")
	}
}

func TestSetPathListIndex(t *testing.T) {
	m := map[string]any{
		"hosts": []any{"h1", "h2"},
	}
	if _, err := SetPath(m, "hosts[1]", "H2"); err != nil {
		t.Fatal(err)
	}
	hosts := m["hosts"].([]any)
	if hosts[0] != "h1" || hosts[1] != "H2" {
		t.Fatalf("%v", hosts)
	}
	// 越界自动扩容
	if _, err := SetPath(m, "hosts[4]", "h5"); err != nil {
		t.Fatal(err)
	}
	if len(m["hosts"].([]any)) != 5 {
		t.Fatalf("扩容失败: %v", m["hosts"])
	}
	// 下标段后接子路径
	if _, err := SetPath(m, "items[0].name", "n"); err != nil {
		t.Fatal(err)
	}
	items := m["items"].([]any)
	if items[0].(map[string]any)["name"] != "n" {
		t.Fatalf("%#v", items)
	}
}

func TestSetPathErrors(t *testing.T) {
	m := map[string]any{"a": "scalar", "l": []any{1}}
	for _, p := range []string{"a.b", "l[0].x", "", "a..b", "a[", "a[x]", "a[0][1]", "a."} {
		if _, err := SetPath(m, p, 1); err == nil {
			t.Fatalf("路径 %q 应报错", p)
		}
	}
	// l[0] 是标量 1，接子路径应报错
	if _, err := SetPath(m, "l[0].x", 1); err == nil {
		t.Fatal("标量元素接子路径应报错")
	}
}

func TestParseSetInfer(t *testing.T) {
	cases := []struct {
		in   string
		key  string
		want any
	}{
		{"a=1", "a", int64(1)},
		{"a=1.5", "a", 1.5},
		{"a=true", "a", true},
		{"a=false", "a", false},
		{"a=null", "a", nil},
		{"a=hello", "a", "hello"},
		{"a=", "a", ""},
		{"a.b[0]=v2", "a.b[0]", "v2"},
	}
	for _, c := range cases {
		k, v, err := ParseSet(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if k != c.key || !reflect.DeepEqual(v, c.want) {
			t.Fatalf("%s: got %q=%#v want %q=%#v", c.in, k, v, c.key, c.want)
		}
	}
	if _, _, err := ParseSet("novalue"); err == nil {
		t.Fatal("缺 = 应报错")
	}
}

func TestApplySetEndToEnd(t *testing.T) {
	m := map[string]any{}
	var err error
	for _, pair := range []string{"app.name=demo", "app.replicas=3", "app.hosts[0]=h1", "app.debug=true"} {
		if m, err = ApplySet(m, pair); err != nil {
			t.Fatal(err)
		}
	}
	app := m["app"].(map[string]any)
	if app["name"] != "demo" || app["replicas"] != int64(3) || app["debug"] != true {
		t.Fatalf("%#v", app)
	}
	if app["hosts"].([]any)[0] != "h1" {
		t.Fatalf("%#v", app["hosts"])
	}
}
