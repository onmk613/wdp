package cli

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"wdp/internal/chart"
	"wdp/internal/config"
	"wdp/internal/model"
	"wdp/internal/render"
)

const renderHelp = `
chart 静态渲染预览（不执行、不连主机）

输出三段：合并后的 values（defaults + -f/--set 深合并）、全部模板文件的渲染
结果、deploy 任务清单（chart 引用展开一层，与执行侧同口径）
渲染用样例域：合并 values + --hostname 占位主机名（默认 preview-host）
预览不加载 inventory：声明了 inventory_override 的键实际值可能被 inventory
同名变量覆盖，此处只反映 chart values 口径
要看"真机现状下会改什么"用 wdp run --check（连主机、只读探测 + 变更预估）

示例：
wdp render ./myapp
wdp render ./myapp -f envs/prod.yaml --hostname web1
`

// newRenderCmd 构造 `wdp render`：chart 静态渲染预览（不执行；执行侧
// 预演用 wdp run --check）。
func newRenderCmd() *cobra.Command {
	var (
		hostname             string
		valuesFiles, setArgs []string
	)
	cmd := &cobra.Command{
		Use:   "render <chart-dir|tgz>",
		Short: "preview merged values, rendered templates and the task list (no execution)",
		Long:  renderHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ch, values, eng, err := chart.OpenWithLimits(args[0], valuesFiles, setArgs, chart.Limits{MaxExtractBytes: config.Current().MaxExtractBytes()})
			if err != nil {
				return err
			}
			defer ch.Close()

			out := cmd.OutOrStdout()
			sample := sampleDomain(values, hostname)
			fmt.Fprintf(out, "chart %s %s\n\n", ch.Meta.Name, ch.Meta.Version)
			// 预览不加载 inventory：声明了 inventory_override 的键实际值可能
			// 被 inventory 同名变量覆盖，此处显式提示预览口径，避免误读。
			if len(ch.Meta.InventoryOverride) > 0 {
				fmt.Fprintf(out, "note: inventory_override %v — inventory same-name vars beat values for these keys at runtime (this preview shows chart values only)\n\n", ch.Meta.InventoryOverride)
			}

			fmt.Fprintln(out, "========== VALUES (merged) ==========")
			b, _ := yaml.Marshal(values)
			fmt.Fprint(out, string(b))

			for _, rel := range ch.TemplateFiles() {
				data, err := os.ReadFile(filepath.Join(ch.Dir, rel))
				if err != nil {
					fmt.Fprintf(out, "========== %s (read failed) ==========\n%v\n", rel, err)
					continue
				}
				fmt.Fprintf(out, "========== %s ==========\n", rel)
				rendered, err := eng.Render(string(data), sample)
				if err != nil {
					fmt.Fprintf(out, "!! render failed: %v\n", err)
					continue
				}
				fmt.Fprint(out, rendered)
				if !strings.HasSuffix(rendered, "\n") {
					fmt.Fprintln(out)
				}
			}

			fmt.Fprintln(out, "========== TASKS ==========")
			for _, p := range ch.Deploy {
				fmt.Fprintf(out, "play [%s] hosts=%s\n", p.Name, p.Hosts)
				printTasks(out, p.Tasks, sample, eng, ch, "")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&hostname, "hostname", "preview-host",
		"inventory_hostname placeholder for preview")
	chartValueFlags(cmd, &valuesFiles, &setArgs)
	return cmd
}

// printTasks 打印任务清单（chart 引用展开一层，free-form 渲染预览）。
// chart 引用的 vars 同样并入子作用域（与执行侧 expand 一致，预览不漂移）。
func printTasks(out io.Writer, tasks []*model.Task, domain map[string]any, eng *render.Engine, ch *chart.Chart, indent string) {
	for _, t := range tasks {
		fmt.Fprintf(out, "%s- %s (%s)\n", indent, t.Label(), moduleLabel(t))
		if t.ChartRef != "" {
			// 与执行/lint 统一走 ResolveSub：支持版本约束引用（common@1.x）
			sub, err := ch.ResolveSub(t.ChartRef)
			if err != nil {
				fmt.Fprintf(out, "%s    !! %s\n", indent, err.Error())
				continue
			}
			// 入口 play 与执行侧一致：缺省 deploy，tasks_from 选其他相位
			sp, perr := sub.EntryPlay(t.TasksFrom)
			if perr != nil {
				fmt.Fprintf(out, "%s    !! %s\n", indent, perr.Error())
				continue
			}
			scope := chart.SubScope(sub, domain)
			maps.Copy(scope, t.ChartVars)
			printTasks(out, sp.Tasks, scope, eng, ch, indent+"    ")
			continue
		}
		if t.FreeForm != "" {
			if s, err := eng.Render(t.FreeForm, domain); err == nil {
				fmt.Fprintf(out, "%s    cmd: %s\n", indent, s)
			}
		}
	}
}

func moduleLabel(t *model.Task) string {
	if t.ChartRef != "" {
		return "chart:" + t.ChartRef
	}
	return t.Module
}

func sampleDomain(values map[string]any, hostname string) map[string]any {
	sample := map[string]any{}
	maps.Copy(sample, values)
	sample["inventory_hostname"] = hostname
	return sample
}
