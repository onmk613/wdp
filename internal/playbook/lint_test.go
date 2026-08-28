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
