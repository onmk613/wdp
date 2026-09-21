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
// hosts 可选（裸任务相位/简单 play 不写，执行侧缺省）；hosts: null 归入
// 空（不再产生垃圾串）；hosts 列表形态仍报错（不支持）。
func TestParsePlayEmptyAndBadDocs(t *testing.T) {
	if plays, err := Parse([]byte("# only comments\n")); err != nil || len(plays) != 0 {
		t.Fatalf("空文档: %v %v", plays, err)
	}
	if _, err := Parse([]byte("hosts: all\n")); err == nil {
		t.Fatal("顶层非列表应报错")
	}
	plays, err := Parse([]byte("- hosts:\n"))
	if err != nil || len(plays) != 1 || plays[0].Hosts != "" {
		t.Fatalf("hosts 缺失应允许（执行侧缺省）: %v %v", plays, err)
	}
	if _, err := Parse([]byte("- hosts: [a, b]\n")); err == nil {
		t.Fatal("hosts 列表形态应报错（不支持，旧版会静默产生垃圾串）")
	}
}

// TestParseBareTasks 裸任务形态：序列项直接是任务（无 play 包装），
// 连续裸任务合并为一个隐式 play（hosts 空）；与 play 形态可混用。
func TestParseBareTasks(t *testing.T) {
	plays, err := Parse([]byte("- name: 获取主机架构\n  setup:\n- name: 装包\n  package: nginx\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plays) != 1 {
		t.Fatalf("连续裸任务应合并为一个隐式 play: %d", len(plays))
	}
	p := plays[0]
	if p.Hosts != "" {
		t.Fatalf("隐式 play 的 hosts 应为空（执行侧缺省）: %q", p.Hosts)
	}
	if len(p.Tasks) != 2 || p.Tasks[0].Module != "setup" || p.Tasks[1].Module != "package" {
		t.Fatalf("任务解析异常: %+v", p.Tasks)
	}
	if p.Tasks[0].Name != "获取主机架构" || p.Tasks[1].Name != "装包" {
		t.Fatalf("任务名丢失: %+v", p.Tasks)
	}

	// 混用：裸任务 → play → 裸任务（各自独立）
	plays, err = Parse([]byte("- shell: 'echo a'\n- name: p1\n  hosts: web\n  tasks:\n    - shell: 'echo b'\n- shell: 'echo c'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plays) != 3 {
		t.Fatalf("混用形态应拆为 3 个 play: %d", len(plays))
	}
	if plays[0].Hosts != "" || plays[1].Hosts != "web" || plays[2].Hosts != "" {
		t.Fatalf("hosts 异常: %q %q %q", plays[0].Hosts, plays[1].Hosts, plays[2].Hosts)
	}
}

// TestParseTasksFrom tasks_from 是 chart 引用任务的入口相位选择属性：
// 仅 chart 引用可用，值必须是非空字符串。
func TestParseTasksFrom(t *testing.T) {
	plays, err := Parse([]byte(`
- hosts: all
  tasks:
    - chart: {name: jdk}
      tasks_from: validate
`))
	if err != nil {
		t.Fatal(err)
	}
	if t0 := plays[0].Tasks[0]; t0.TasksFrom != "validate" || t0.Module != "chart" {
		t.Fatalf("tasks_from 解析: %+v", t0)
	}
	// 普通模块任务上使用应报错
	if _, err := Parse([]byte("- hosts: all\n  tasks:\n    - shell: x\n      tasks_from: validate\n")); err == nil {
		t.Fatal("shell 任务不应允许 tasks_from")
	}
	// 空串 / 非字符串
	if _, err := Parse([]byte("- hosts: all\n  tasks:\n    - chart: {name: jdk}\n      tasks_from: ''\n")); err == nil {
		t.Fatal("tasks_from 空串应报错")
	}
	if _, err := Parse([]byte("- hosts: all\n  tasks:\n    - chart: {name: jdk}\n      tasks_from: [a]\n")); err == nil {
		t.Fatal("tasks_from 非字符串应报错")
	}
}

// TestParseInvalidIntFieldsError retries/delay/timeout 的非法值必须报错，
// 不得静默得 0（=重试关闭/无延迟）：与 parseSerial 的非法值报错口径一致，
// 拼写错误（"abc"、布尔）当场暴露而非运行时行为悄悄变化。
func TestParseInvalidIntFieldsError(t *testing.T) {
	bad := map[string]string{
		"retries": `- hosts: all
  tasks:
    - shell: x
      retries: abc
`,
		"delay": `- hosts: all
  tasks:
    - shell: x
      delay: true
`,
		"timeout": `- hosts: all
  tasks:
    - shell: x
      timeout: [3]
`,
	}
	for field, src := range bad {
		if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s 非法值应报错且错误信息含字段名, got: %v", field, err)
		}
	}
	// 数值字符串仍是合法写法
	plays, err := Parse([]byte("- hosts: all\n  tasks:\n    - shell: x\n      retries: \"3\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if plays[0].Tasks[0].Retries != 3 {
		t.Fatalf("retries 数值字符串应解析为 3: %+v", plays[0].Tasks[0])
	}
}

// TestParseStrategyGateInvalidIntsError gate 的 retries/delay 非法值同样报错。
func TestParseStrategyGateInvalidIntsError(t *testing.T) {
	if _, err := Parse([]byte(`
- hosts: all
  strategy:
    type: rolling
    gate:
      shell: 'true'
      retries: abc
  tasks:
    - shell: 'true'
`)); err == nil || !strings.Contains(err.Error(), "gate retries") {
		t.Fatalf("gate retries 非法值应报错: %v", err)
	}
	if _, err := Parse([]byte(`
- hosts: all
  strategy:
    type: rolling
    gate:
      shell: 'true'
      delay: abc
  tasks:
    - shell: 'true'
`)); err == nil || !strings.Contains(err.Error(), "gate delay") {
		t.Fatalf("gate delay 非法值应报错: %v", err)
	}
}
