package chart

// ModuleNames 的对账测试：按相位分组、子 chart 引用展开、block 组递归、
// 非内置模块（chart 本地脚本模块）不计入。

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestModuleNamesSubChart 展开：父 chart 的 template + 子 chart 的 shell
// （含 handler）都计入 deploy 相位；无 uninstall.yaml 则该相位不出现。
func TestModuleNamesSubChart(t *testing.T) {
	c, err := Load(writeChart(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	m := c.ModuleNames()
	got, ok := m["deploy"]
	if !ok {
		t.Fatalf("deploy 相位应有模块集: %#v", m)
	}
	if !slices.Equal(got, []string{"shell", "template"}) {
		t.Fatalf("deploy 模块集 = %v, want [shell template]", got)
	}
	if _, ok := m["uninstall"]; ok {
		t.Fatal("无 uninstall.yaml 不应出现该相位")
	}
}

// TestModuleNamesBlockAndPhases block 组递归 + 多相位各自独立收集。
func TestModuleNamesBlockAndPhases(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("chart.yaml", "name: blk\nversion: 0.1.0\n")
	mustWrite("values.yaml", "{}\n")
	mustWrite("deploy.yaml", `
- hosts: all
  tasks:
    - name: 块内组
      block:
        - name: 建目录
          file: {path: /tmp/x, state: directory}
      rescue:
        - name: 报告
          debug: {msg: failed}
      always:
        - name: 收尾
          set_fact: {done: "1"}
    - name: 本地脚本模块不计入
      mylocalmod: foo=1
`)
	mustWrite("uninstall.yaml", `
- hosts: all
  tasks:
    - name: 卸载
      file: {path: /tmp/x, state: absent}
`)
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	m := c.ModuleNames()
	if !slices.Equal(m["deploy"], []string{"debug", "file", "set_fact"}) {
		t.Fatalf("deploy 模块集 = %v, want [debug file set_fact]（block/rescue/always 递归、非内置不计）", m["deploy"])
	}
	if !slices.Equal(m["uninstall"], []string{"file"}) {
		t.Fatalf("uninstall 模块集 = %v, want [file]", m["uninstall"])
	}
}
