package cli

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"wdp/internal/fmtutil"
	"wdp/internal/i18n"
	"wdp/internal/inventory"
)

// inventoryHelp 返回 `wdp inventory` 的长帮助（调用时求值）。
func inventoryHelp() string {
	return i18n.T(`List hosts and groups from the inventory

The table prints host name, address, port, connection type (conn) and group memberships
The optional positional argument is a host pattern (default all): comma union, ! prefix exclusion,
:& intersection chains, path.Match wildcards (matched against both group and host names; group hits
expand to their members)
--vars additionally prints each host's merged variables (YAML, including the layer-by-layer group vars overrides)
Multiple -i inventories are shown here as merged

Examples:
wdp inventory
wdp inventory 'webservers:&production'
wdp inventory web1 --vars
`, `列出 inventory 中的主机与组

表格输出主机名、地址、端口、连接类型（conn）、所属组
可选位置参数为主机模式（默认 all）：逗号联合、! 前缀排除、:& 交集链、
path.Match 通配（同时匹配组名与主机名，组命中展开成员）
--vars 额外打印每主机合并后的变量（YAML，含组 vars 逐层覆盖结果）
多个 -i 清单在此按合并后口径展示

示例：
wdp inventory
wdp inventory 'webservers:&production'
wdp inventory web1 --vars
`)
}

// newInventoryCmd 构造 `wdp inventory`。
func newInventoryCmd() *cobra.Command {
	var showVars bool
	cmd := &cobra.Command{
		Use:     "inv [host-pattern]",
		Aliases: []string{"inventory", "i"},
		Short: i18n.T("list hosts/groups from the inventory",
			"列出 inventory 中的主机与组"),
		Long: inventoryHelp(),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := "all"
			if len(args) == 1 {
				pattern = args[0]
			}
			inv, err := loadInventories()
			if err != nil {
				return err
			}
			return runInventoryList(inv, pattern, showVars, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&showVars, "vars", false, "print merged vars per host (YAML)")
	return cmd
}

// runInventoryList 渲染清单（命令体与测试共用内核）。
func runInventoryList(inv *inventory.Inventory, pattern string, showVars bool, out io.Writer) error {
	hosts, err := inv.Select(pattern)
	if err != nil {
		return err
	}
	groups := inv.GroupsMap() // 组名 → 成员主机名
	hostGroups := map[string][]string{}
	for g, members := range groups {
		if g == "all" {
			continue
		}
		for _, m := range members {
			hostGroups[m] = append(hostGroups[m], g)
		}
	}

	if showVars {
		for _, h := range hosts {
			fmt.Fprintf(out, "========== %s ==========\n", h.Name)
			b, err := yaml.Marshal(h.Vars)
			if err != nil {
				return err
			}
			if _, err := out.Write(b); err != nil {
				return err
			}
		}
		return nil
	}

	tb := fmtutil.NewTable("HOST", "ADDRESS", "PORT", "CONN", "GROUPS")
	for _, h := range hosts {
		gs := hostGroups[h.Name]
		slices.Sort(gs)
		tb.AddRow(fmtutil.C(h.Name), fmtutil.C(h.Address),
			fmtutil.C(strconv.Itoa(h.Port)), fmtutil.C(h.Conn), fmtutil.C(strings.Join(gs, ",")))
	}
	tb.RenderTo(out)
	fmt.Fprintf(out, "%d host(s)\n", len(hosts))
	return nil
}
