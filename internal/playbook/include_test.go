package playbook

// include 片段展开测试：基本展开、嵌套相对路径、环引用拦截、
// 与模块键互斥、坏路径报错。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestIncludeExpands 静态展开：片段任务按顺序拼接进主任务列表。
func TestIncludeExpands(t *testing.T) {
	root := writeTree(t, map[string]string{
		"site.yaml": `
- name: 主
  hosts: webservers
  tasks:
    - name: 前
      shell: 'before'
    - include: tasks/frag.yaml
    - name: 后
      shell: 'after'
`,
		"tasks/frag.yaml": `
- name: 片段一
  shell: 'frag-1'
- name: 片段二
  shell: 'frag-2'
`,
	})
	plays, err := Load(filepath.Join(root, "site.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, t := range plays[0].Tasks {
		labels = append(labels, t.Label())
	}
	got := strings.Join(labels, ",")
	want := "前,片段一,片段二,后"
	if got != want {
		t.Fatalf("展开结果 %q, 期望 %q", got, want)
	}
}

// TestIncludeNestedRelativePath 嵌套 include 以片段自身目录解析相对路径。
func TestIncludeNestedRelativePath(t *testing.T) {
	root := writeTree(t, map[string]string{
		"site.yaml": `
- hosts: all
  tasks:
    - include: tasks/a.yaml
`,
		"tasks/a.yaml": `
- name: A
  shell: 'a'
- include: b.yaml      # 相对 tasks/ 目录
`,
		"tasks/b.yaml": `
- name: B
  shell: 'b'
`,
	})
	plays, err := Load(filepath.Join(root, "site.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plays[0].Tasks) != 2 || plays[0].Tasks[1].Name != "B" {
		t.Fatalf("嵌套展开: %#v", plays[0].Tasks)
	}
}

// TestIncludeCycleDetected 环引用（a→b→a）在深度上限内被拦截并报错。
func TestIncludeCycleDetected(t *testing.T) {
	root := writeTree(t, map[string]string{
		"site.yaml": `
- hosts: all
  tasks:
    - include: a.yaml
`,
		"a.yaml": `
- include: b.yaml
`,
		"b.yaml": `
- include: a.yaml
`,
	})
	_, err := Load(filepath.Join(root, "site.yaml"))
	if err == nil || !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("环引用应报深度错误, got: %v", err)
	}
}

// TestIncludeMissingFile 片段缺失在加载期 fail-loud。
func TestIncludeMissingFile(t *testing.T) {
	root := writeTree(t, map[string]string{
		"site.yaml": `
- hosts: all
  tasks:
    - include: nope.yaml
`,
	})
	_, err := Load(filepath.Join(root, "site.yaml"))
	if err == nil || !strings.Contains(err.Error(), "nope.yaml") {
		t.Fatalf("缺失片段应报错, got: %v", err)
	}
}

// TestIncludeWithModuleRejected include 不得与其他模块键共存。
func TestIncludeWithModuleRejected(t *testing.T) {
	_, err := Parse([]byte(`
- hosts: all
  tasks:
    - include: tasks/x.yaml
      shell: 'oops'
`))
	if err == nil || !strings.Contains(err.Error(), "include cannot be combined") {
		t.Fatalf("include+module 应报错, got: %v", err)
	}
}

// TestIncludeInsideBlock block 子树内同样展开。
func TestIncludeInsideBlock(t *testing.T) {
	root := writeTree(t, map[string]string{
		"site.yaml": `
- hosts: all
  tasks:
    - name: 容错组
      block:
        - include: tasks/frag.yaml
      rescue:
        - name: 恢复
          shell: 'rescue'
`,
		"tasks/frag.yaml": `
- name: 片段
  shell: 'frag'
`,
	})
	plays, err := Load(filepath.Join(root, "site.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	blk := plays[0].Tasks[0]
	if len(blk.Block) != 1 || blk.Block[0].Name != "片段" {
		t.Fatalf("block 内 include 未展开: %#v", blk.Block)
	}
}

// TestIncludeAttrsPropagated import 语义：include 上的 when/tags/become
// 下沉到每个片段任务（when AND 合并，片段自身声明优先级更高）。
func TestIncludeAttrsPropagated(t *testing.T) {
	root := writeTree(t, map[string]string{
		"site.yaml": `
- hosts: all
  tasks:
    - include: tasks/frag.yaml
      when: '{{ .smart }}'
      tags: [smart]
      become: true
`,
		"tasks/frag.yaml": `
- name: 无自身条件
  shell: 'a'
- name: 自带条件
  shell: 'b'
  when: '{{ .installed }}'
`,
	})
	plays, err := Load(filepath.Join(root, "site.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	a, b := plays[0].Tasks[0], plays[0].Tasks[1]
	if len(a.When) != 1 || a.When[0] != "{{ .smart }}" {
		t.Fatalf("when 未传播: %#v", a.When)
	}
	if len(b.When) != 2 { // include 条件 AND 片段自身条件
		t.Fatalf("when 未 AND 合并: %#v", b.When)
	}
	if len(a.Tags) != 1 || a.Tags[0] != "smart" {
		t.Fatalf("tags 未传播: %#v", a.Tags)
	}
	if a.Become == nil || !*a.Become {
		t.Fatalf("become 未下沉: %#v", a.Become)
	}
}

// TestIncludeLoopRejected include 不支持 loop（静态展开无运行期项）。
func TestIncludeLoopRejected(t *testing.T) {
	root := writeTree(t, map[string]string{
		"site.yaml": `
- hosts: all
  tasks:
    - include: tasks/frag.yaml
      loop: [a, b]
`,
		"tasks/frag.yaml": `
- shell: 'x'
`,
	})
	_, err := Load(filepath.Join(root, "site.yaml"))
	if err == nil || !strings.Contains(err.Error(), "does not support loop") {
		t.Fatalf("include+loop 应报错, got: %v", err)
	}
}
