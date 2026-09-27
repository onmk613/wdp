package chart

// 审查修复回归测试：chart.yaml version 校验（打包产物名逃逸）、charts/
// 下 .tgz 子 chart 显式报错、模板目录列举失败上报 lint、tgz 未支持条目
// 类型显式报错、Merge/SubScope 深拷贝隔离、--set 前导零保持字符串。

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateMetaVersion version 拼进打包产物文件名（<name>-<version>.tgz），
// 路径分隔符/点开头的值经 filepath.Join 逃出 outDir——加载入口拒绝；
// 正常 semver 与缺省不受影响。
func TestValidateMetaVersion(t *testing.T) {
	bad := []string{"../../x", "..", ".", "a/../b", "/abs", "a/b", "./x"}
	for _, v := range bad {
		if err := validateMeta(&Meta{Name: "x", Version: v}); err == nil {
			t.Errorf("version %q 应被拒绝", v)
		}
	}
	good := []string{"", "1.0.0", "0.1.0", "1.0.0-beta.1", "17", "v2.3.4"}
	for _, v := range good {
		if err := validateMeta(&Meta{Name: "x", Version: v}); err != nil {
			t.Errorf("version %q 不应被拒绝: %v", v, err)
		}
	}
}

// writeMinimalChart 写出可加载的最小 chart 目录（可选追加文件）。
func writeMinimalChart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("chart.yaml", "name: demo\nversion: 0.1.0\n")
	write("deploy.yaml", "- name: p\n  hosts: all\n  tasks: []\n")
	return dir
}

// TestLoadRejectsPackagedSubchart Helm 惯例的 charts/foo-1.0.0.tgz 被
// 静默跳过后，引用处只能得到 "subchart not found"——现在显式报错并指明
// 改用目录形式。
func TestLoadRejectsPackagedSubchart(t *testing.T) {
	dir := writeMinimalChart(t)
	tgz := filepath.Join(dir, "charts", "foo-1.0.0.tgz")
	if err := os.MkdirAll(filepath.Dir(tgz), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tgz, []byte("not really a tgz"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("charts/ 下的 .tgz 子 chart 应显式报错")
	}
	if !strings.Contains(err.Error(), "packaged subcharts (.tgz) are not supported") {
		t.Fatalf("错误应指明不支持打包子 chart 并给出改法: %v", err)
	}
}

// TestLintReportsUnlistableTemplates templates/ 存在但列不出文件（权限）
// 必须是 lint ERROR：旧实现吞掉 walk 错误，lint 全绿、渲染时才发现模板
// 缺失。root 用户不受目录权限约束，跳过。
func TestLintReportsUnlistableTemplates(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受目录读权限约束")
	}
	dir := writeMinimalChart(t)
	tpl := filepath.Join(dir, "templates")
	if err := os.MkdirAll(tpl, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tpl, "app.conf.tpl"), []byte("x={{ .b }}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tpl, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tpl, 0o755) })

	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	files, werr := c.walkTemplates()
	if werr == nil {
		t.Fatalf("目录不可读应返回错误, files=%v", files)
	}
	var hit bool
	for _, iss := range Lint(c, map[string]any{"b": "1"}) {
		if iss.Level == ERROR && strings.Contains(iss.Msg, "failed to list templates") {
			hit = true
		}
	}
	if !hit {
		t.Fatal("lint 应对 templates 列举失败报 ERROR")
	}
}

// writeTgzWithHardlink 生成含 hardlink 条目的最小 chart tgz。
func writeTgzWithHardlink(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "demo-0.1.0.tgz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	add := func(hdr *tar.Header, body string) {
		t.Helper()
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	chartYAML := "name: demo\nversion: 0.1.0\n"
	add(&tar.Header{Name: "demo/chart.yaml", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(chartYAML))}, chartYAML)
	deploy := "- name: p\n  hosts: all\n  tasks: []\n"
	add(&tar.Header{Name: "demo/deploy.yaml", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(deploy))}, deploy)
	// hardlink：合法 tar 条目但 wdp 不支持
	add(&tar.Header{Name: "demo/linked.conf", Typeflag: tar.TypeLink, Mode: 0o644, Linkname: "demo/chart.yaml"}, "")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadTgzRejectsUnsupportedEntry hardlink/FIFO 等合法 tar 但未支持的
// 条目类型必须显式报错（含条目名与类型），不再静默丢弃。
func TestLoadTgzRejectsUnsupportedEntry(t *testing.T) {
	_, err := Load(writeTgzWithHardlink(t))
	if err == nil {
		t.Fatal("含 hardlink 条目的包应拒绝加载")
	}
	for _, want := range []string{"demo/linked.conf", "hardlink", "unsupported type"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误应包含 %q: %v", want, err)
		}
	}
}

// TestMergeIsolation 回归：Merge 结果与 base/override 不得共享嵌套
// map/列表——改子 map 即污染入参源（deepCopyValues 注释记录过此类事故）。
func TestMergeIsolation(t *testing.T) {
	base := map[string]any{
		"a": map[string]any{"x": 1},
		"l": []any{map[string]any{"k": "v"}},
	}
	override := map[string]any{
		"a": map[string]any{"y": 2},
		"b": map[string]any{"z": 3},
	}
	m := Merge(base, override)

	// 改合并结果的子 map / 列表元素
	m["a"].(map[string]any)["y"] = 99
	m["a"].(map[string]any)["x"] = 77
	m["b"].(map[string]any)["z"] = 88
	m["l"].([]any)[0].(map[string]any)["k"] = "polluted"

	if got := override["a"].(map[string]any)["y"]; got != 2 {
		t.Errorf("override 子 map 被污染: y=%v", got)
	}
	if got := base["a"].(map[string]any)["x"]; got != 1 {
		t.Errorf("base 子 map 被污染: x=%v", got)
	}
	if got := override["b"].(map[string]any)["z"]; got != 3 {
		t.Errorf("override 新增子 map 被污染: z=%v", got)
	}
	if got := base["l"].([]any)[0].(map[string]any)["k"]; got != "v" {
		t.Errorf("base 列表内 map 被污染: k=%v", got)
	}
}

// TestSubScopeMergeIsolation SubScope 出口的 <子chart名> 子树来自父作用域，
// 修改子 chart 作用域的子 map 不得写回父作用域。
func TestSubScopeMergeIsolation(t *testing.T) {
	sub := &Chart{Meta: Meta{Name: "jdk", Version: "1.0.0"}, Values: map[string]any{
		"own": map[string]any{"x": 1},
	}}
	parent := map[string]any{
		"jdk":    map[string]any{"cfg": map[string]any{"y": 2}},
		"global": map[string]any{"g": map[string]any{"z": 3}},
	}
	scope := SubScope(sub, parent)

	scope["cfg"].(map[string]any)["y"] = 99
	scope["global"].(map[string]any)["g"].(map[string]any)["z"] = 88
	scope["own"].(map[string]any)["x"] = 77

	if got := parent["jdk"].(map[string]any)["cfg"].(map[string]any)["y"]; got != 2 {
		t.Errorf("父作用域 <jdk> 子树被污染: y=%v", got)
	}
	if got := parent["global"].(map[string]any)["g"].(map[string]any)["z"]; got != 3 {
		t.Errorf("父作用域 global 被污染: z=%v", got)
	}
	if got := sub.Values["own"].(map[string]any)["x"]; got != 1 {
		t.Errorf("子 chart 默认 values 被污染: x=%v", got)
	}
}

// TestInferTypeLeadingZero 回归：纯数字带前导零保持字符串——
// --set mode=0755 的八进制写法是用户意图，ParseInt 推成 755 会丢掉
// 八进制语义（argMode 按字符串 "0…" 前缀解析）；无前导零的数字照常
// 推断为数字。
func TestInferTypeLeadingZero(t *testing.T) {
	if got := inferType("0755"); got != "0755" {
		t.Errorf(`inferType("0755") = %#v, want "0755"（字符串）`, got)
	}
	if got := inferType("00"); got != "00" {
		t.Errorf(`inferType("00") = %#v, want "00"（字符串）`, got)
	}
	if got := inferType("8080"); got != int64(8080) {
		t.Errorf(`inferType("8080") = %#v, want int64(8080)`, got)
	}
	if got := inferType("0"); got != int64(0) {
		t.Errorf(`inferType("0") = %#v, want int64(0)（单个 0 不是八进制写法）`, got)
	}
	if got := inferType("0.5"); got != 0.5 {
		t.Errorf(`inferType("0.5") = %#v, want 0.5（浮点不受影响）`, got)
	}

	// 端到端：ApplySet 后 mode 保持 "0755"、port 仍是数字
	values := map[string]any{}
	if _, err := ApplySet(values, "mode=0755"); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySet(values, "port=8080"); err != nil {
		t.Fatal(err)
	}
	if got := values["mode"]; got != "0755" {
		t.Errorf("mode=0755 应保持字符串 %#v", got)
	}
	if got := values["port"]; got != int64(8080) {
		t.Errorf("port=8080 应是数字 %#v", got)
	}
}
