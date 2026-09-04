package cli

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"wdp/internal/fmtutil"
	"wdp/internal/release"
)

// newReleaseCmd 构造 `wdp release`（部署记录）。
func newReleaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release",
		Short: "deployment record viewing",
	}
	cmd.AddCommand(newReleaseListCmd(), newReleaseShowCmd(), newReleaseDiffCmd(), newReleaseDelCmd())
	return cmd
}

// newReleaseListCmd 构造 `wdp release list`。
func newReleaseListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list [chart-name prefix]",
		Short: "list deployment records (newest first)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			filter := ""
			if len(args) > 0 {
				filter = args[0]
			}
			recs, err := release.List(filter)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(recs) == 0 {
				fmt.Fprintln(out, "(no records)")
				return nil
			}
			tb := outPrinter(cmd).NewTable("ID", "VERSION", "RESULT", "TIME")
			for _, r := range recs {
				if r.Failed {
					tb.AddRow(fmtutil.C(r.ID), fmtutil.C(r.Version),
						fmtutil.CC("failed", fmtutil.Red), fmtutil.C(r.Time.Format("2006-01-02 15:04:05")))
				} else {
					tb.AddRow(fmtutil.C(r.ID), fmtutil.C(r.Version),
						fmtutil.CC("ok", fmtutil.Green), fmtutil.C(r.Time.Format("2006-01-02 15:04:05")))
				}
			}
			tb.Render()
			return nil
		},
	}
}

const releaseShowHelp = `
查看单条部署记录详情

输出 ID、时间、chart 与版本、结果、主机清单、values 文件引用与快照
--values 只打印 values 快照 YAML——可直接 -f 回放复现该次部署参数
裸 playbook 记录无 values 快照

示例：
wdp release show myapp-1760000000000000123
wdp release show myapp-1760000000000000123 --values > prod.yaml
`

// newReleaseShowCmd 构造 `wdp release show`。
func newReleaseShowCmd() *cobra.Command {
	var asValues bool
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "show record details",
		Long:  releaseShowHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, err := release.Load(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asValues {
				if rec.Values == nil {
					return fmt.Errorf("this record has no values (bare playbook mode)")
				}
				return printYAML(out, rec.Values)
			}
			fmt.Fprintf(out, "ID:     %s\nTime:  %s\nChart:  %s %s\nResult:  %s\nHosts:  %v\n",
				rec.ID, rec.Time.Format("2006-01-02 15:04:05"), rec.Chart, rec.Version,
				boolLabel(rec.Failed), rec.Hosts)
			if len(rec.ValuesRef) > 0 {
				fmt.Fprintf(out, "Args:  %v\n", rec.ValuesRef)
			}
			if rec.Values != nil {
				fmt.Fprintln(out, "\nvalues snapshot (--values for the full content):")
				if err := printYAML(out, rec.Values); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asValues, "values", false,
		"print only the values snapshot (YAML)")
	return cmd
}

// newReleaseDiffCmd 构造 `wdp release diff`。
func newReleaseDiffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <id1> <id2>",
		Short: "diff the values snapshots of two deployment records",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := release.Load(args[0])
			if err != nil {
				return err
			}
			b, err := release.Load(args[1])
			if err != nil {
				return err
			}
			if a.Values == nil || b.Values == nil {
				return fmt.Errorf("both sides need a values snapshot (bare playbook records cannot be compared)")
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "diff %s (%s %s) -> %s (%s %s)\n",
				a.ID, a.Chart, a.Version, b.ID, b.Chart, b.Version)
			lines := release.DiffValues(a.Values, b.Values)
			if len(lines) == 0 {
				fmt.Fprintln(out, "values identical (no parameter changes)")
				return nil
			}
			for _, l := range lines {
				fmt.Fprintln(out, l)
			}
			return nil
		},
	}
}

const releaseDelHelp = `
删除部署记录（本地审计文件，可一条或多条）

默认按 ID 删除：ID 可只给前缀（记录 ID 形如 <chart>-<unixnano>，完整抄录
不便），唯一命中才删，歧义报候选；全部参数先解析，任一无命中或歧义则不删任何记录
--prefix 批量前缀：每个参数命中的所有记录都删（按 chart 名清旧记录）
--regex 批量正则：参数为 Go 正则对 ID 匹配（非锚定，^...$ 控制全串）
批量模式先列出将删除的清单，--yes 确认后才执行；删除不可恢复

示例：
wdp release del myapp-1760001234                      # 唯一前缀，直接删
wdp release del myapp-1760001234 other-998            # 多条
wdp release del --prefix myapp --yes                  # 该 chart 全部记录
wdp release del --regex '^myapp-1760001' --yes        # 正则批量
`

// newReleaseDelCmd 构造 `wdp release del`。
func newReleaseDelCmd() *cobra.Command {
	var byPrefix, byRegex, yes bool
	cmd := &cobra.Command{
		Use:   "del <id>... [--prefix | --regex] [--yes]",
		Short: "delete one or more deployment records (unique ID prefix; --prefix/--regex bulk with --yes)",
		Long:  releaseDelHelp,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bulk := byPrefix || byRegex
			if byPrefix && byRegex {
				return fmt.Errorf("%s", "--prefix and --regex are mutually exclusive")
			}
			recs, err := release.List("")
			if err != nil {
				return err
			}
			var targets []*release.Record
			if bulk {
				targets, err = selectRecordPatterns(recs, args, byRegex)
			} else {
				targets, err = resolveRecordIDs(recs, args)
			}
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// 批量删除影响面大（一个前缀/正则可命中全部记录）：先列清单，
			// --yes 确认后才动手（与 agentctl retire 同约定）
			if bulk && !yes {
				fmt.Fprintf(out, "about to delete %d record(s):\n", len(targets))
				for _, r := range targets {
					fmt.Fprintf(out, "  %s (%s, %s, %s)\n", r.ID, recordLabel(r),
						r.Time.Format("2006-01-02 15:04"), boolLabel(r.Failed))
				}
				fmt.Fprintln(out, "This is irreversible. Pass --yes to confirm.")
				return nil
			}
			for _, r := range targets {
				if err := release.Delete(r.ID); err != nil {
					return err
				}
				fmt.Fprintf(out, "deleted %s (%s, %s, %s)\n",
					r.ID, recordLabel(r), r.Time.Format("2006-01-02 15:04"), boolLabel(r.Failed))
			}
			fmt.Fprintf(out, "%d record(s) deleted\n", len(targets))
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&byPrefix, "prefix", false,
		"bulk: delete ALL records whose ID starts with each arg")
	f.BoolVar(&byRegex, "regex", false,
		"bulk: args are Go regexes matched against record IDs (anchor with ^... as needed)")
	f.BoolVarP(&yes, "yes", "y", false,
		"confirm bulk (--prefix/--regex) deletion without the review listing")
	return cmd
}

// recordLabel 渲染记录摘要标签（chart 版本；裸 playbook 无 chart 名）。
func recordLabel(r *release.Record) string {
	if what := strings.TrimSpace(r.Chart + " " + r.Version); what != "" {
		return what
	}
	return "(bare playbook)"
}

// selectRecordPatterns 批量模式（--prefix/--regex）选取全部命中记录：
// --prefix 为 ID 前缀匹配；--regex 为 Go 正则（非锚定）。多参数取并集，
// 零命中报错。正则先全部编译（坏表达式在任何删除前失败）。
func selectRecordPatterns(recs []*release.Record, args []string, regex bool) ([]*release.Record, error) {
	var regexes []*regexp.Regexp
	if regex {
		for _, a := range args {
			re, err := regexp.Compile(a)
			if err != nil {
				return nil, fmt.Errorf("bad regex %q: %w", a, err)
			}
			regexes = append(regexes, re)
		}
	}
	var out []*release.Record
	for _, r := range recs {
		hit := false
		if regex {
			for _, re := range regexes {
				if re.MatchString(r.ID) {
					hit = true
					break
				}
			}
		} else {
			for _, a := range args {
				if strings.HasPrefix(r.ID, a) {
					hit = true
					break
				}
			}
		}
		if hit {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no records match %s", strings.Join(args, ", "))
	}
	return out, nil
}

// resolveRecordIDs 把用户给定的 ID（完整或前缀）解析为待删记录（去重、
// 保序）。精确命中优先；否则按前缀匹配，唯一命中才通过——多条报错列出
// 候选，零命中报错；任一参数失败即整体失败（删除开始前调用，保证不全删）。
func resolveRecordIDs(recs []*release.Record, args []string) ([]*release.Record, error) {
	byID := make(map[string]*release.Record, len(recs))
	for _, r := range recs {
		byID[r.ID] = r
	}
	seen := map[string]bool{}
	var out []*release.Record
	for _, arg := range args {
		r, ok := byID[arg]
		if !ok {
			var matches []*release.Record
			for _, cand := range recs {
				if strings.HasPrefix(cand.ID, arg) {
					matches = append(matches, cand)
				}
			}
			if len(matches) != 1 {
				return nil, fmt.Errorf("%q matches %d record(s)%s", arg, len(matches), candidateIDs(matches))
			}
			r = matches[0]
		}
		if !seen[r.ID] {
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	return out, nil
}

// candidateIDs 渲染候选 ID 列表（歧义时报给用户，用更长前缀消歧）。
func candidateIDs(matches []*release.Record) string {
	if len(matches) == 0 {
		return ""
	}
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, m.ID)
	}
	return "; candidates: " + strings.Join(ids, ", ")
}

// printYAML 序列化输出 YAML（部署记录 values 快照用）。
func printYAML(out io.Writer, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	_, err = out.Write(b)
	return err
}

func boolLabel(failed bool) string {
	if failed {
		return "failed"
	}
	return "ok"
}
