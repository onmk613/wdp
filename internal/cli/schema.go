package cli

// `wdp schema`：YAML 结构字段速查——inventory 主机条目与 playbook 任务
// 控制属性的"骨架"参考，写清单/playbook 时不用翻文档。区别于具体模块
// 的参数文档（那是 `wdp module` 的职责）。字段表与解析器同源
// （playbook.TaskFieldSections / inventory.HostFieldSections），对账测试
// 保证永不漂移。

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"wdp/internal/chart"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/playbook"
)

const schemaHelp = `
YAML 结构字段速查（写 inventory / playbook / chart 前的骨架参考）

不带参数：总览——结构字段域在前，具体模块参数另见 wdp module
host：inventory 主机条目可用的全部连接参数键（地址/认证/TLS/提权/通道专属），
  未列出的键一律视为主机变量（模板经 .hostvars 访问）
task：playbook 任务控制属性全表（条件/循环/重试/提权/委托/呈现/容错块），
  按语义分组并附可直接粘贴的示例片段
chart：chart.yaml 全部字段 + 生命周期相位属性（release/record/clears_marker/
  values_from），按语义分组并附示例

字段表与解析器同源（新增字段不写文档过不了对账测试），永不漂移；
具体模块（shell/copy/template...）的参数文档用 wdp module <模块名>

示例：
wdp schema                    # 总览
wdp schema host               # 主机条目字段
wdp schema task               # 任务控制属性
wdp schema chart              # chart.yaml 字段
wdp schema task --json        # 机器可读（编辑器插件/脚本）
`

// newSchemaCmd 构造 `wdp schema`。
func newSchemaCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "schema [host|task|chart]",
		Short: "YAML structure field reference: host entries, task attributes and chart.yaml",
		Long:  schemaHelp,
		Args:  cobra.MaximumNArgs(1),
		ValidArgsFunction: func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			// 结构字段域在前（骨架先于具体模块）；host/task/chart 之后各域有
			// 独立文档，模块参数不在本命令（wdp module）
			var out []string
			for _, c := range []struct{ name, desc string }{
				{"host", "inventory 主机条目连接参数键"},
				{"task", "playbook 任务控制属性"},
				{"chart", "chart.yaml 字段与相位属性"},
			} {
				if strings.HasPrefix(c.name, toComplete) {
					out = append(out, c.name+"\t"+c.desc)
				}
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				// --json 在总览下没有可序列化的对象：显式报错而不是静默忽略
				if asJSON {
					return fmt.Errorf("--json requires a schema domain (options: host | task | chart)")
				}
				printSchemaOverview(out)
				return nil
			}
			var sections []model.FieldSection
			switch args[0] {
			case "host":
				sections = inventory.HostFieldSections()
			case "task":
				sections = playbook.TaskFieldSections()
			case "chart":
				sections = chart.ChartFieldSections()
			default:
				return fmt.Errorf("unknown schema domain %q (options: host | task | chart; module parameters: `wdp module <name>`)", args[0])
			}
			if asJSON {
				b, err := json.MarshalIndent(sections, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			printFieldSections(out, args[0], sections)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable JSON (editors/scripts)")
	return cmd
}

// printSchemaOverview 总览：结构字段域在前，与具体模块文档区分。
func printSchemaOverview(out io.Writer) {
	fmt.Fprint(out, `YAML structure field reference（结构字段速查）

  schema host    inventory 主机条目连接参数键（地址/认证/TLS/提权/通道专属）
  schema task    playbook 任务控制属性（条件/循环/重试/委托/容错块）
  schema chart   chart.yaml 字段与生命周期相位属性

模块参数（shell/copy/template... 的入参）不在本命令：
  wdp module            全部内置模块列表
  wdp module <name>     单个模块的参数与示例

字段表与解析器同源（对账测试防漂移），详见 docs/03、docs/05、docs/08。
`)
}

// printFieldSections 渲染一个域的字段分组表：组标题 + 对齐字段行 + 示例片段。
func printFieldSections(out io.Writer, domain string, sections []model.FieldSection) {
	fmt.Fprintf(out, "%s fields\n\n", domain)
	for _, sec := range sections {
		fmt.Fprintf(out, "== %s ==\n", sec.Title)
		for _, f := range sec.Fields {
			fmt.Fprintf(out, "  %s %s %s %s\n",
				padDisplay(f.Name, 21), padDisplay(f.Type, 13), padDisplay(f.Default, 26), f.Desc)
		}
		if sec.Example != "" {
			fmt.Fprintf(out, "  example:\n%s", indentLines(sec.Example, "    "))
		}
		fmt.Fprintln(out)
	}
}

// padDisplay 按终端显示宽度右补空格（CJK/全角字符占 2 列，Go 的 %-Ns 按
// rune 计宽会错位，此处自行计宽对齐）。
func padDisplay(s string, width int) string {
	w := 0
	for _, r := range s {
		w += runeDisplayWidth(r)
	}
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// runeDisplayWidth 近似 East Asian Width：CJK/全角/韩文按 2 列，其余 1 列。
func runeDisplayWidth(r rune) int {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0xA4CF, // CJK 部首..彝文
		r >= 0xAC00 && r <= 0xD7A3, // 韩文音节
		r >= 0xF900 && r <= 0xFAFF, // CJK 兼容表意
		r >= 0xFE30 && r <= 0xFE4F, // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60, // 全角形式（（）等）
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000 && r <= 0x3FFFD: // CJK 扩展
		return 2
	}
	return 1
}

func indentLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n") + "\n"
}
