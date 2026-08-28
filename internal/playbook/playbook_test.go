package playbook

import (
	"reflect"
	"strings"
	"testing"

	"wdp/internal/model"
)

const sample = `
- name: web 部署
  hosts: webservers
  vars:
    port: 8080
  become: true
  environment:
    LANG: C
  tasks:
    - name: 安装 nginx
      package:
        name: nginx
        state: present
      notify:
        - 重载 nginx
      tags: [install]

    - name: 简写命令
      shell: uptime

    - name: 带循环与条件
      copy:
        src: "conf/{{ .item }}.conf"
        dest: "/etc/{{ .item }}.conf"
      loop: ["a", "b"]
      when: port is defined
      register: cp_result
      ignore_errors: true
      retries: 3
      delay: 5

  handlers:
    - name: 重载 nginx
      service:
        name: nginx
        state: reloaded
`

func TestParsePlaybook(t *testing.T) {
	plays, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(plays) != 1 {
		t.Fatalf("play 数 %d", len(plays))
	}
	p := plays[0]
	if p.Name != "web 部署" || p.Hosts != "webservers" || !p.Become {
		t.Fatalf("play: %+v", p)
	}
	if p.Vars["port"] != 8080 {
		t.Fatalf("vars: %+v", p.Vars)
	}
	if p.Environment["LANG"] != "C" {
		t.Fatalf("environment: %+v", p.Environment)
	}
	if len(p.Tasks) != 3 || len(p.Handlers) != 1 {
		t.Fatalf("tasks=%d handlers=%d", len(p.Tasks), len(p.Handlers))
	}

	t1 := p.Tasks[0]
	if t1.Module != "package" || t1.Args["name"] != "nginx" || t1.Args["state"] != "present" {
		t.Fatalf("task1: %+v", t1)
	}
	if len(t1.Notify) != 1 || t1.Notify[0] != "重载 nginx" {
		t.Fatalf("task1 notify: %v", t1.Notify)
	}
	if len(t1.Tags) != 1 || t1.Tags[0] != "install" {
		t.Fatalf("task1 tags: %v", t1.Tags)
	}

	t2 := p.Tasks[1]
	if t2.Module != "shell" || t2.FreeForm != "uptime" {
		t.Fatalf("task2: %+v", t2)
	}

	t3 := p.Tasks[2]
	if t3.Module != "copy" || len(t3.Loop) != 2 {
		t.Fatalf("task3: %+v", t3)
	}
	if t3.Register != "cp_result" || !t3.IgnoreErrors || t3.Retries != 3 || t3.DelaySec != 5 {
		t.Fatalf("task3: %+v", t3)
	}
	if len(t3.When) != 1 {
		t.Fatalf("task3 when: %v", t3.When)
	}

	h := p.Handlers[0]
	if !h.IsHandler || h.Module != "service" || h.Args["state"] != "reloaded" {
		t.Fatalf("handler: %+v", h)
	}
}

func TestParseMultipleModulesError(t *testing.T) {
	_, err := Parse([]byte("- hosts: all\n  tasks:\n    - shell: x\n      copy: {dest: /y}\n"))
	if err == nil {
		t.Fatal("多模块应报错")
	}
}

func TestParseNoModuleError(t *testing.T) {
	_, err := Parse([]byte("- hosts: all\n  tasks:\n    - name: 只有名字\n"))
	if err == nil {
		t.Fatal("无模块应报错")
	}
}

func TestParseExplicitArgs(t *testing.T) {
	plays, err := Parse([]byte(`
- hosts: all
  tasks:
    - copy:
      args:
        content: hello
        dest: /tmp/x
`))
	if err != nil {
		t.Fatal(err)
	}
	task := plays[0].Tasks[0]
	if task.Module != "copy" || task.Args["content"] != "hello" || task.Args["dest"] != "/tmp/x" {
		t.Fatalf("task: %+v", task)
	}
}

// TestPlayKeysCoverModelTags playKeys（任务级模块键排除用）必须覆盖
// model.Play 全部 yaml tag 键与手工解析特殊键——新增字段打 tag 后漏更
// playKeys 会把新键误判为任务模块名，此测试提前拦截。
func TestPlayKeysCoverModelTags(t *testing.T) {
	tt := reflect.TypeFor[model.Play]()
	for field := range tt.Fields() {
		tag := field.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if !playKeys[name] {
			t.Fatalf("model.Play 字段 %s 的 yaml 键 %q 未登记进 playKeys", field.Name, name)
		}
	}
	for _, k := range []string{"become", "serial", "strategy", "tasks", "handlers"} {
		if !playKeys[k] {
			t.Fatalf("手工解析键 %s 未登记进 playKeys", k)
		}
	}
}

// TestParsePlayScalarCoercion tag 映射路径的标量宽容性：数值/布尔风格
// 标量解进 string 字段、数值解进 map[string]string 值（旧 fmt.Sprint 语义）。
func TestParsePlayScalarCoercion(t *testing.T) {
	plays, err := Parse([]byte(`
- name: 2024
  hosts: all
  environment: {PORT: 8080}
  tasks:
    - shell: echo hi
`))
	if err != nil {
		t.Fatal(err)
	}
	p := plays[0]
	if p.Name != "2024" || p.Environment["PORT"] != "8080" {
		t.Fatalf("标量宽容性回归: %+v", p)
	}
}

// TestParsePlayEmptyAndBadDocs 边界：空文档零 play；顶层非列表报错；
// hosts 缺失报错（hosts: null 现在也归入缺失，不再产生垃圾串）。
func TestParsePlayEmptyAndBadDocs(t *testing.T) {
	if plays, err := Parse([]byte("# only comments\n")); err != nil || len(plays) != 0 {
		t.Fatalf("空文档: %v %v", plays, err)
	}
	if _, err := Parse([]byte("hosts: all\n")); err == nil {
		t.Fatal("顶层非列表应报错")
	}
	if _, err := Parse([]byte("- hosts:\n")); err == nil {
		t.Fatal("hosts 缺失应报错")
	}
	if _, err := Parse([]byte("- hosts: [a, b]\n")); err == nil {
		t.Fatal("hosts 列表形态应报错（不支持，旧版会静默产生垃圾串）")
	}
}
