package module

import (
	"fmt"
	"strings"

	"wdp/internal/model"
)

func init() {
	Register(&AddHostModule{})
}

// AddHostModule 运行期向 inventory 新增（或更新）主机，可同时入组。
//
//   - name: 注册新节点
//     add_host:
//     name: node5
//     address: 10.8.2.105   # 缺省等于 name
//     port: 22
//     conn: ssh             # ssh | agent | local（与 inventory 主机字段同语义）
//     groups: [etcd]
//     vars: {zone: az2}
//
// 典型用法：bootstrap play 里按运行时结果注册节点（如扩容清单来自外部查询），
// 后续 play 用 `hosts: etcd` 直接编排新成员。聚合时机与 group_by 一致：
// 下一批次/play 的选择期与 groups/hosts/hostvars 内置变量可见。
type AddHostModule struct{}

// Name 模块名。
func (m *AddHostModule) Name() string { return "add_host" }

// Desc 模块说明。
func (m *AddHostModule) Desc() string {
	return "add or update an inventory host at runtime, optionally joining groups (visible to later plays)"
}

// Run 构造 HostAddition（name 必填；其余字段覆盖缺省 Host）。
func (m *AddHostModule) Run(_ *RunContext, args map[string]any, _ string) *Result {
	name, _ := argStr(args, "name")
	if strings.TrimSpace(name) == "" {
		return Fail("%s", "add_host requires a host name")
	}
	h := &model.Host{Name: name}
	if addr, ok := argStr(args, "address"); ok && addr != "" {
		h.Address = addr
	} else {
		h.Address = name
	}
	if port, ok := argInt(args, "port"); ok && port > 0 {
		h.Port = port
	}
	if user, ok := argStr(args, "user"); ok && user != "" {
		h.User = user
	}
	if conn, ok := argStr(args, "conn"); ok && conn != "" {
		h.Conn = conn
	}
	if url, ok := argStr(args, "agent_url"); ok && url != "" {
		h.AgentURL = url
	}
	h.Vars = map[string]any{}
	if vars, ok := args["vars"].(map[string]any); ok {
		for k, v := range vars {
			h.Vars[k] = v
		}
	}
	groups, _ := argStrList(args, "groups")
	return &Result{
		Changed: true,
		AddHost: &HostAddition{Host: h, Groups: groups},
		Msg:     fmt.Sprintf("registered host %s (%s)%s", name, h.Address, groupsSuffix(groups)),
	}
}

func groupsSuffix(groups []string) string {
	if len(groups) == 0 {
		return ""
	}
	return " groups: " + strings.Join(groups, ",")
}

// Params 参数文档。
func (m *AddHostModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "string", Desc: "host name (required, used as inventory_hostname)"},
		{Name: "address", Type: "string", Desc: "connection address (defaults to name)"},
		{Name: "port", Type: "int", Desc: "ssh/agent port (defaults to 22)"},
		{Name: "user", Type: "string", Desc: "ssh user"},
		{Name: "conn", Type: "string", Desc: "connection type: ssh | agent | local"},
		{Name: "agent_url", Type: "string", Desc: "agent service url when conn=agent"},
		{Name: "groups", Type: "list", Desc: "groups to join (later plays can select them)"},
		{Name: "vars", Type: "map", Desc: "host vars (visible via .hostvars and templates on that host)"},
	}
}

// Example 示例任务。
func (m *AddHostModule) Example() string {
	return `- name: register a scaled-out node from a runtime query
  shell: 'cat /etc/mycluster/pending-node'   # e.g. prints 10.8.2.105
  register: pending

- name: add it to the cluster group
  add_host:
    name: 'node-{{ .pending.stdout | trim }}'
    address: '{{ .pending.stdout | trim }}'
    groups: [etcd]
    vars: {zone: az2}

- name: configure it like the rest
  …  # hosts: etcd in the next play now includes the new node
`
}
