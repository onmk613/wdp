package module

import (
	"fmt"
	"strings"

	"wdp/internal/i18n"
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
//     agent_port: 7602
//     conn: agent           # agent | local（与 inventory 主机字段同语义）
//     groups: [etcd]
//     vars: {zone: az2}
//
// 典型用法：bootstrap play 里按运行时结果注册节点（如扩容清单来自外部查询），
// 后续 play 用 `hosts: etcd` 直接编排新成员。聚合时机与 group_by 一致：
// 下一批次/play 的选择期与 groups/hosts/hostvars 内置变量可见。
type AddHostModule struct{}

func (m *AddHostModule) Name() string { return "add_host" }

func (m *AddHostModule) Desc() string {
	return i18n.T("Add or update inventory hosts at runtime (optionally joining groups); immediately visible to selectors in later plays", "运行期新增/更新 inventory 主机（可同时入组），后续 play 的选择器立即可见")
}

// Run 构造 HostAddition（name 必填；其余字段覆盖缺省 Host）。
func (m *AddHostModule) Run(_ *RunContext, args map[string]any, _ string) *Result {
	name, _ := argStr(args, "name")
	if strings.TrimSpace(name) == "" {
		return Fail("add_host requires a host name")
	}
	h := &model.Host{Name: name}
	if addr, ok := argStr(args, "address"); ok && addr != "" {
		h.Address = addr
	} else {
		h.Address = name
	}
	if port, ok := argInt(args, "agent_port"); ok && port > 0 {
		h.AgentPort = port
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

func (m *AddHostModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "string", Desc: i18n.T("host name (required, i.e. inventory_hostname)", "主机名（必填，即 inventory_hostname）")},
		{Name: "address", Type: "string", Desc: i18n.T("connection address (defaults to name)", "连接地址（缺省同 name）")},
		{Name: "agent_port", Type: "int", Desc: i18n.T("agent port (defaults to wdp.cfg [agent].port / 7602)", "agent 端口（缺省取 wdp.cfg [agent].port / 7602）")},
		{Name: "conn", Type: "string", Enum: []string{"agent", "local"}, Desc: i18n.T("connection channel type", "连接通道类型")},
		{Name: "agent_url", Type: "string", Desc: i18n.T("agent service address (used when conn=agent)", "agent 服务地址（conn=agent 时用）")},
		{Name: "groups", Type: "list", Desc: i18n.T("groups to join as well (later plays can select by group)", "同时加入的组（后续 play 可按组选择）")},
		{Name: "vars", Type: "map", Desc: i18n.T("host variables (visible on that host via .hostvars and templates)", "主机变量（该主机上经 .hostvars 与模板可见）")},
	}
}

func (m *AddHostModule) Example() string {
	return i18n.T(`- name: Pick up the scale-out node from a runtime query
  shell: 'cat /etc/mycluster/pending-node'   # e.g. prints 10.8.2.105
  register: pending

- name: Register it into the cluster group
  add_host:
    name: 'node-{{ .pending.stdout | trim }}'
    address: '{{ .pending.stdout | trim }}'
    groups: [etcd]
    vars: {zone: az2}

- name: Configure it the same way as existing nodes
  …  # the next play's hosts: etcd already includes the new node
`, `- name: 从运行期查询拿到扩容节点
  shell: 'cat /etc/mycluster/pending-node'   # 例如输出 10.8.2.105
  register: pending

- name: 登记进集群组
  add_host:
    name: 'node-{{ .pending.stdout | trim }}'
    address: '{{ .pending.stdout | trim }}'
    groups: [etcd]
    vars: {zone: az2}

- name: 与既有节点同套路配置
  …  # 下个 play 的 hosts: etcd 已包含新节点
`)
}
