package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"wdp/internal/chart"
	"wdp/internal/i18n"
	"wdp/internal/model"
	"wdp/internal/render"
)

// newTemplateCmd 构造 `wdp template`。
func newTemplateCmd() *cobra.Command {
	var (
		hostname             string
		valuesFiles, setArgs []string
	)
	cmd := &cobra.Command{
		Use: "template <chart目录|tgz>",
		Short: i18n.T("preview merged values, rendered templates and the task list (no execution)",
			"预览合并 values、模板渲染结果与任务清单（不执行）"),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ch, values, eng, err := chart.Open(args[0], valuesFiles, setArgs)
			if err != nil {
				return err
			}
			defer ch.Close()

			out := cmd.OutOrStdout()
			sample := sampleDomain(values, hostname)
			fmt.Fprintf(out, "chart %s %s\n\n", ch.Meta.Name, ch.Meta.Version)

			fmt.Fprintln(out, "========== VALUES（合并后） ==========")
			b, _ := yaml.Marshal(values)
			fmt.Fprint(out, string(b))

			for _, rel := range ch.TemplateFiles() {
				data, err := os.ReadFile(filepath.Join(ch.Dir, rel))
				if err != nil {
					fmt.Fprintf(out, "========== %s（读取失败） ==========\n%v\n", rel, err)
					continue
				}
				fmt.Fprintf(out, "========== %s ==========\n", rel)
				rendered, err := eng.Render(string(data), sample)
				if err != nil {
					fmt.Fprintf(out, "!! 渲染失败: %v\n", err)
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
		i18n.T("inventory_hostname placeholder for preview", "预览用 inventory_hostname 占位"))
	chartValueFlags(cmd, &valuesFiles, &setArgs)
	return cmd
}

// printTasks 打印任务清单（chart 引用展开一层，free-form 渲染预览）。
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
	for k, v := range values {
		sample[k] = v
	}
	sample["inventory_hostname"] = hostname
	return sample
}

// newLintCmd 构造 `wdp lint`。
func newLintCmd() *cobra.Command {
	var valuesFiles, setArgs []string
	cmd := &cobra.Command{
		Use:   "lint <chart目录|tgz>",
		Short: i18n.T("statically validate a chart", "静态校验 chart"),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ch, values, _, err := chart.Open(args[0], valuesFiles, setArgs)
			if err != nil {
				return err
			}
			defer ch.Close()

			out := cmd.OutOrStdout()
			errCount := 0
			for _, is := range chart.Lint(ch, values) {
				fmt.Fprintln(out, is)
				if is.Level == chart.ERROR {
					errCount++
				}
			}
			if errCount > 0 {
				return fmt.Errorf("chart %s: %d 个错误", ch.Meta.Name, errCount)
			}
			fmt.Fprintf(out, "%s %s %s: %s\n", "chart", ch.Meta.Name, ch.Meta.Version,
				i18n.T("validation passed", "校验通过"))
			return nil
		},
	}
	chartValueFlags(cmd, &valuesFiles, &setArgs)
	return cmd
}

// newPackageCmd 构造 `wdp package`。
func newPackageCmd() *cobra.Command {
	var outDir string
	cmd := &cobra.Command{
		Use:   "package <chart目录>",
		Short: i18n.T("package a chart into <name>-<version>.tgz", "打包 chart 为 <name>-<version>.tgz"),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := chart.Package(args[0], outDir)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), out)
			return nil
		},
	}
	cmd.Flags().StringVarP(&outDir, "output", "o", ".", i18n.T("output directory", "输出目录"))
	return cmd
}
