package chart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBranchChart 构造跨分支同名子 chart：charts/a/charts/lib 与
// charts/b/charts/lib 同名（不同版本）。
func writeBranchChart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("chart.yaml", "name: root\nversion: 1.0.0\n")
	must("values.yaml", "{}\n")
	must("deploy.yaml", "- hosts: all\n  tasks:\n    - chart: a\n")
	must("charts/a/chart.yaml", "name: a\nversion: 1.0.0\n")
	must("charts/a/values.yaml", "{}\n")
	must("charts/a/deploy.yaml", "- hosts: all\n  tasks:\n    - chart: lib@^1.0\n")
	must("charts/a/charts/lib/chart.yaml", "name: lib\nversion: 1.0.0\n")
	must("charts/a/charts/lib/values.yaml", "{}\n")
	must("charts/a/charts/lib/deploy.yaml", "- hosts: all\n  tasks:\n    - shell: 'a-lib'\n")
	must("charts/b/chart.yaml", "name: b\nversion: 1.0.0\n")
	must("charts/b/values.yaml", "{}\n")
	must("charts/b/deploy.yaml", "- hosts: all\n  tasks:\n    - chart: lib@^9.0\n")
	must("charts/b/charts/lib/chart.yaml", "name: lib\nversion: 9.9.9\n")
	must("charts/b/charts/lib/values.yaml", "{}\n")
	must("charts/b/charts/lib/deploy.yaml", "- hosts: all\n  tasks:\n    - shell: 'b-lib'\n")
	return dir
}

// TestResolveSubLexicalScope §1.4 回归：词法作用域优先——分支内引用 lib
// 命中自身的 charts/lib（版本约束按正确实例校验），不再被字典序靠前的
// 其他分支同名子 chart 判死。
func TestResolveSubLexicalScope(t *testing.T) {
	c, err := Load(writeBranchChart(t))
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.ResolveSub("a")
	if err != nil {
		t.Fatal(err)
	}
	// a 分支内解析 lib@^1.0：命中 a 自己的 lib（1.0.0），不是 b 的 9.9.9
	lib, err := a.ResolveSub("lib@^1.0")
	if err != nil {
		t.Fatalf("词法作用域内 lib@^1.0 应命中自身实例: %v", err)
	}
	if lib.Meta.Version != "1.0.0" {
		t.Fatalf("命中错误实例: %s", lib.Meta.Version)
	}
	b, _ := c.ResolveSub("b")
	if _, err := b.ResolveSub("lib@^9.0"); err != nil {
		t.Fatalf("b 分支内 lib@^9.0 应命中 9.9.9: %v", err)
	}
}

// TestFindSubAmbiguous：从根跨分支按名查找 lib 是歧义的——报错并列出
// 候选路径，不按字典序静默选一个。
func TestFindSubAmbiguous(t *testing.T) {
	c, err := Load(writeBranchChart(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.FindSub("lib")
	if err == nil {
		t.Fatal("跨分支同名应报错")
	}
	for _, want := range []string{"ambiguous", "root.a.lib", "root.b.lib"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误应包含 %q: %v", want, err)
		}
	}
	// 根自身的直接子 chart 无歧义
	if _, err := c.FindSub("a"); err != nil {
		t.Fatalf("直接子 chart 查找不应报错: %v", err)
	}
	// 单一深层命中不报错：根下没有其他同名 a
	a, err := c.FindSub("b")
	if err != nil || a.Meta.Name != "b" {
		t.Fatalf("单一命中: %v", err)
	}
}
