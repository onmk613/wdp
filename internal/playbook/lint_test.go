package playbook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLint 裸 playbook 静态检查：未知模块/缺模块/chart 引用均报 ERROR，
// 合法任务通过。
func TestLint(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	os.WriteFile(good, []byte(`- hosts: all
  tasks:
    - name: t1
      shell: echo hi
`), 0o644)
	if issues := Lint(good); len(issues) != 0 {
		t.Fatalf("合法 playbook 不应有发现: %+v", issues)
	}

	bad := filepath.Join(dir, "bad.yaml")
	os.WriteFile(bad, []byte(`- hosts: all
  tasks:
    - name: typo
      userr: state=present
    - name: sub
      chart_ref: ./sub
`), 0o644)
	issues := Lint(bad)
	if len(issues) != 2 {
		t.Fatalf("应报 2 个问题（未知模块 + chart 引用），实际 %+v", issues)
	}
	for _, is := range issues {
		if is.Level != "ERROR" {
			t.Fatalf("均应为 ERROR: %+v", is)
		}
	}
}

// TestLintBareVar 裸变量引用（Ansible 习语 {{ var }}）必须在 lint
// 阶段拦截并给出前导点提示，而不是等到运行时才炸。
func TestLintBareVar(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bare.yaml")
	os.WriteFile(p, []byte(`- hosts: all
  tasks:
    - name: 获取容器ID
      shell: docker ps | awk '{print $1}'
      register: doris_container_id
    - name: 删除core文件
      shell: |
        docker exec -i {{ doris_container_id.stdout }} bash -c "rm -rf core.*"
`), 0o644)
	issues := Lint(p)
	found := false
	for _, is := range issues {
		if strings.Contains(is.Msg, `function "doris_container_id" not defined`) &&
			strings.Contains(is.Msg, "leading dot") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应拦截裸变量引用并附提示: %+v", issues)
	}
}

// TestLintBareModuleCall 模块键写了但底下没有任何参数（`set_fact:`
// 后内容缺失）必须 lint 期拦截：这类任务运行期在全部主机上失败，用户
// 只能看到汇总报错，定位不到具体任务。
func TestLintBareModuleCall(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bare.yaml")
	os.WriteFile(p, []byte(`- hosts: all
  tasks:
    - name: 获取主机详细信息
      set_fact:
    - name: 采集后使用
      shell: echo {{ .arch }}
`), 0o644)
	issues := Lint(p)
	found := false
	for _, is := range issues {
		if is.Level == "ERROR" && strings.Contains(is.Msg, `task "获取主机详细信息"`) &&
			strings.Contains(is.Msg, "set_fact requires at least one key/value pair") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应发现空 set_fact 任务: %+v", issues)
	}

	// setup 裸调用合法（采集 facts），不应误报
	ok := filepath.Join(dir, "ok.yaml")
	os.WriteFile(ok, []byte(`- hosts: all
  tasks:
    - name: 获取主机详细信息
      setup:
`), 0o644)
	if issues := Lint(ok); len(issues) != 0 {
		t.Fatalf("setup 裸调用不应有发现: %+v", issues)
	}
}

// TestLintContent 内存内容级校验：合法通过、未知模块/语法错误为 ERROR、
// chart 引用跳过目录检查（无文件上下文）。
func TestLintContent(t *testing.T) {
	if is := LintContent([]byte("- name: ok\n  shell: echo hi\n")); len(is) != 0 {
		t.Fatalf("合法内容应无发现: %+v", is)
	}
	is := LintContent([]byte("- name: bad\n  nosuchmod: {}\n"))
	if len(is) == 0 || is[0].Level != "ERROR" {
		t.Fatalf("未知模块应为 ERROR: %+v", is)
	}
	is = LintContent([]byte("- name: ref\n  chart:\n    name: anything\n"))
	if len(is) != 0 {
		t.Fatalf("无文件上下文时 chart 引用不应报目录缺失: %+v", is)
	}
	is = LintContent([]byte(":::not yaml"))
	if len(is) == 0 || is[0].Level != "ERROR" {
		t.Fatalf("语法错误应为 ERROR: %+v", is)
	}
}
