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
	"wdp/internal/fmtutil"
	"wdp/internal/model"
	"wdp/internal/module"
	"wdp/internal/playbook"
	"wdp/internal/render"
	"wdp/internal/skel"
)

// newTemplateCmd 构造 `wdp template` 命令组：chart 的静态侧工具——
// new 生成骨架、module 查内置模块文档、render 预览渲染（执行侧预演用
// wdp run --check，两者分层见 docs/12）。
func newTemplateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "template",
		Short: "chart static toolset: new / module / render",
	}
	cmd.AddCommand(newTemplateNewCmd(), newTemplateModuleCmd(), newTemplateRenderCmd())
	return cmd
}

// newTemplateModuleCmd 构造 `wdp template module`（原 wdp modules）：
// 无参列出全部内置模块，带名输出参数文档与示例片段。
func newTemplateModuleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "module [module-name]",
		Short: "list built-in modules (with a name, print parameter docs and example)",

		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeModuleNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 1 {
				snippet, err := skel.ModuleSnippet(args[0])
				if err != nil {
					return err
				}
				fmt.Fprint(out, snippet)
				return nil
			}
			tb := outPrinter(cmd).NewTable("MODULE", "DESCRIPTION")
			for _, name := range module.Names() {
				m, _ := module.Get(name)
				tb.AddRow(fmtutil.C(name), fmtutil.C(m.Desc()))
			}
			tb.Render()
			return nil
		},
	}
}

// completeModuleNames 补全内置模块名（cobra 约定：候选以 "name\tdesc"
// 形式返回，zsh/fish 补全菜单展示描述；首个参数已给出后不再补全）。
func completeModuleNames(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, name := range module.Names() {
		if strings.HasPrefix(name, toComplete) {
			if m, ok := module.Get(name); ok {
				out = append(out, name+"\t"+m.Desc())
			}
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// newTemplateRenderCmd 构造 `wdp template render`（原 wdp template）：
// chart 静态渲染预览（不执行；执行侧预演用 wdp run --check）。
func newTemplateRenderCmd() *cobra.Command {
	var (
		hostname             string
		valuesFiles, setArgs []string
	)
	cmd := &cobra.Command{
		Use:   "render <chart-dir|tgz>",
		Short: "preview merged values, rendered templates and the task list (no execution)",

		Args: cobra.ExactArgs(1),
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
			scope := chart.SubScope(sub, domain)
			maps.Copy(scope, t.ChartVars)
			for _, sp := range sub.Deploy {
				printTasks(out, sp.Tasks, scope, eng, ch, indent+"    ")
			}
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

// newLintCmd 构造 `wdp lint`。
func newLintCmd() *cobra.Command {
	var valuesFiles, setArgs []string
	cmd := &cobra.Command{
		Use:   "lint <chart-dir|tgz|playbook.yaml>",
		Short: "statically validate a chart or a bare playbook",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			// 裸 playbook：只做任务树静态检查（模块名/结构/chart 引用拒绝）
			if strings.HasSuffix(args[0], ".yaml") || strings.HasSuffix(args[0], ".yml") {
				errCount := 0
				for _, is := range playbook.Lint(args[0]) {
					fmt.Fprintf(out, "[%s] %s: %s\n", is.Level, is.Path, is.Msg)
					if is.Level == "ERROR" {
						errCount++
					}
				}
				if errCount > 0 {
					return fmt.Errorf("playbook %s: %d error(s)", args[0], errCount)
				}
				fmt.Fprintf(out, "playbook %s: validation passed\n", args[0])
				return nil
			}

			ch, values, _, err := chart.OpenWithLimits(args[0], valuesFiles, setArgs, chart.Limits{MaxExtractBytes: config.Current().MaxExtractBytes()})
			if err != nil {
				return err
			}
			defer ch.Close()

			errCount := 0
			for _, is := range chart.Lint(ch, values) {
				fmt.Fprintln(out, is)
				if is.Level == chart.ERROR {
					errCount++
				}
			}
			if errCount > 0 {
				return fmt.Errorf("chart %s: %d error(s)", ch.Meta.Name, errCount)
			}
			fmt.Fprintf(out, "%s %s %s: %s\n", "chart", ch.Meta.Name, ch.Meta.Version,
				"validation passed")
			return nil
		},
	}
	chartValueFlags(cmd, &valuesFiles, &setArgs)
	return cmd
}

// newTemplateNewCmd 构造 `wdp template new`：生成应用包骨架（原 wdp new）。
func newTemplateNewCmd() *cobra.Command {
	var (
		full bool
		dir  string
	)
	cmd := &cobra.Command{
		Use:   "new <app-name>",
		Short: "scaffold a working chart skeleton",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := skel.Scaffold(dir, args[0], full)
			if err != nil {
				return err
			}
			variant := "minimal skeleton"
			if full {
				variant = "full-featured reference skeleton"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "scaffolded %s -> %s\n", variant, root)
			return nil
		},
	}
	cmd.Flags().BoolVar(&full, "full", false,
		"generate a full-featured reference skeleton (strategy/hooks/delegation/dynamic groups/sub-charts)",
	)
	cmd.Flags().StringVarP(&dir, "dir", "d", ".",
		"output directory")
	return cmd
}
