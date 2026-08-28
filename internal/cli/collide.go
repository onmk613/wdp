package cli

// values / inventory 同名碰撞的呈现：分类规则在 internal/chart
// （chart.ValueCollisions）、主机选取在 internal/inventory（SelectPlays），
// 本文件只负责告警/信息的打印格式。

import (
	"fmt"
	"io"
	"slices"

	"wdp/internal/chart"
	"wdp/internal/model"
)

// reportValueCollisions 打印碰撞报告（shadowed 告警 / overridden 信息）。
func reportValueCollisions(w io.Writer, values map[string]any, hosts []*model.Host, allow []string) {
	shadowed, overridden := chart.ValueCollisions(values, hosts, allow)
	if len(shadowed) > 0 {
		fmt.Fprintf(w, "==> warning: inventory variable(s) shadowed by chart values (values always win by design):\n")
		for _, k := range sortedKeys(shadowed) {
			fmt.Fprintf(w, "    %s (hosts: %s) — use a distinct name + dig in templates, or add %q to chart.yaml inventory_override\n",
				k, joinHosts(shadowed[k]), k)
		}
	}
	if len(overridden) > 0 {
		fmt.Fprintf(w, "==> inventory override active (chart.yaml inventory_override):\n")
		for _, k := range sortedKeys(overridden) {
			fmt.Fprintf(w, "    %s (hosts: %s) — inventory value beats chart values for this key\n",
				k, joinHosts(overridden[k]))
		}
	}
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func joinHosts(hosts []string) string {
	out := ""
	for i, h := range hosts {
		if i > 0 {
			out += ","
		}
		out += h
	}
	return out
}
