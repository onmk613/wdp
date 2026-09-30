package module

import (
	"maps"
	"slices"
	"strings"
	"wdp/internal/i18n"
)

func init() {
	Register(&SetFactModule{})
}

// SetFactModule 把键值对写入主机事实库（跨主机可见 + 可持久化 fact cache）。
//
//	set_fact:
//	  node_id: '{{ .inventory_hostname | trunc 2 }}'   # 本机变量域 + fact store
//
// 与 register 的边界：register 结果只进本机变量域；set_fact 同时进入
// 控制端 fact store，后续 play 里其他主机经 .hostvars 可读（两段式
// 编排：收集 play 写 → 配置 play 读）。
type SetFactModule struct{}

func (m *SetFactModule) Name() string { return "set_fact" }

// ReadOnly 只写控制端事实库，不改目标机。
func (m *SetFactModule) ReadOnly() bool { return true }

func (m *SetFactModule) Desc() string {
	return i18n.T("Write key/value pairs into the host fact store (visible across hosts via .hostvars; --fact-cache can persist them)", "写键值对进主机 fact 库（跨主机经 .hostvars 可见，--fact-cache 可持久化）")
}

// Run 把渲染后的参数整表作为 facts 返回（executor 并入本机域与 fact store）。
func (m *SetFactModule) Run(_ *RunContext, args map[string]any, _ string) *Result {
	if len(args) == 0 {
		return Fail("set_fact requires at least one key/value pair")
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return &Result{Facts: maps.Clone(args), Msg: "set facts: " + strings.Join(keys, ", ")}
}

// Params 参数文档。"(any)" 声明接受任意键（ValidateArgs 通配放行）。
func (m *SetFactModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "(any)", Type: "map", Desc: i18n.T("arbitrary key/value pairs (values are template-rendered; merged into the host's variable scope and fact store)", "任意键值对（值经模板渲染；并入本机变量域与 fact 库）")},
	}
}

func (m *SetFactModule) Example() string {
	return i18n.T(`# play 1 (gather): each host derives cluster-wide identifiers
- name: compute the node id
  set_fact:
    node_id: '{{ .inventory_hostname | trunc 2 }}'
    peer_ips: '{{ .play_hosts | join "," }}'

# play 2 (configure): read facts from other hosts
- name: render the cluster config
  template:
    src: templates/app.conf.tpl
    dest: /etc/app/app.conf
# inside app.conf.tpl:
#   peers: {{ range $h := .play_hosts }}{{ (index $.hostvars $h).node_id }} {{ end }}
`, `# play 1（采集）：各主机推导集群级标识
- name: 计算节点 id
  set_fact:
    node_id: '{{ .inventory_hostname | trunc 2 }}'
    peer_ips: '{{ .play_hosts | join "," }}'

# play 2（配置）：读取其他主机的 facts
- name: 渲染集群配置
  template:
    src: templates/app.conf.tpl
    dest: /etc/app/app.conf
# app.conf.tpl 内：
#   peers: {{ range $h := .play_hosts }}{{ (index $.hostvars $h).node_id }} {{ end }}
`)
}
