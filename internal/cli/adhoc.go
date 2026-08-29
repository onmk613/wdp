package cli

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"wdp/internal/config"
	"wdp/internal/conn"
	"wdp/internal/executor"
	"wdp/internal/model"
	"wdp/internal/module"
	"wdp/internal/report"
)

// newAdhocCmd 构造 `wdp adhoc`。
func newAdhocCmd() *cobra.Command {
	var (
		mod, argStr, format string
		become, check, diff bool
	)
	cmd := &cobra.Command{
		Use:   "adhoc -m shell -a 'uptime' <host-pattern>",
		Short: "one-off single-module execution",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := args[0]
			if _, ok := module.Get(mod); !ok {
				return fmt.Errorf("unknown module %q (run `wdp module` for the list)", mod)
			}
			inv, err := loadInventories()
			if err != nil {
				return err
			}
			free, margs := parseAdhocArgs(argStr)
			if diff && !check {
				check = true // --diff 基于 check 只读对比
			}
			play := &model.Play{
				Hosts: pattern, Become: become,
				Tasks: []*model.Task{{Module: mod, FreeForm: free, Args: margs, Become: &become}},
			}
			rep, finish := buildReporter()
			if format != "" {
				// --format：逐主机模板化输出（脚本管道友好），静默其余呈现
				rep = report.NewFormatter(os.Stdout, format)
				finish = func() {}
			}
			conns := conn.NewManagerWithDefaults(connDefaults())
			conns.SetConnectConcurrency(2 * config.Current().Forks())
			ex := executor.New(inv, conns, rep, executor.Options{
				Forks: config.Current().Forks(), TaskTimeout: config.Current().Run.TaskTimeout,
				CheckMode: check, DiffMode: diff,
				MaxDownloadBytes: maxDownloadBytes(),
			})
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			failed := ex.Run(ctx, []*model.Play{play})
			conns.CloseAll()
			finish()
			if failed {
				return errPlayFailed
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&mod, "module", "m", "shell", "module name")
	f.StringVarP(&argStr, "args", "a", "", "module args (free-form or k=v list)")
	f.BoolVarP(&become, "become", "b", false, "escalate privileges")
	f.StringVar(&format, "format", "",
		"format per-host output with a Go template, e.g. '{{.host}}: {{.stdout}}' (fields: .stdout/.stderr/.rc/.changed/.failed/.msg)",
	)
	f.BoolVar(&check, "check", false, "check mode: dry-run without applying changes")
	f.BoolVar(&diff, "diff", false, "diff mode: content-level diff with --check")
	return cmd
}

// parseAdhocArgs 解析 adhoc 参数：含 = 的 token 视为 k=v，其余拼接 free-form。
func parseAdhocArgs(s string) (string, map[string]any) {
	args := map[string]any{}
	var free []string
	for tok := range strings.FieldsSeq(s) {
		if k, v, ok := strings.Cut(tok, "="); ok && k != "" {
			args[k] = v
			continue
		}
		free = append(free, tok)
	}
	return strings.Join(free, " "), args
}
