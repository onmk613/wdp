package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"wdp/internal/fmtutil"
	"wdp/internal/inventory"
)

// newInventoryCmd 构造 `wdp inventory`。
func newInventoryCmd() *cobra.Command {
	var showVars bool
	cmd := &cobra.Command{
		Use:   "inventory [host-pattern]",
		Short: "list hosts/groups from the inventory (merged vars with --vars)",
		Args:  cobra.MaximumNArgs(1),
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
			out.Write(b)
		}
		return nil
	}

	tb := fmtutil.NewTable("HOST", "ADDRESS", "PORT", "CONN", "GROUPS")
	for _, h := range hosts {
		gs := hostGroups[h.Name]
		slices.Sort(gs)
		tb.AddRow(fmtutil.C(h.Name), fmtutil.C(h.Address),
			fmtutil.C(fmt.Sprint(h.Port)), fmtutil.C(h.Conn), fmtutil.C(strings.Join(gs, ",")))
	}
	tb.RenderTo(out)
	fmt.Fprintf(out, "%d host(s)\n", len(hosts))
	return nil
}
