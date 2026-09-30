package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"wdp/internal/config"
	"wdp/internal/conn"
	"wdp/internal/executor"
	"wdp/internal/i18n"
	"wdp/internal/model"
	"wdp/internal/module"
	"wdp/internal/report"
)

// adhocHelp 返回 `wdp adhoc` 的长帮助（调用时求值）。
func adhocHelp() string {
	return i18n.T(`Run a single module once across hosts (no playbook needed)

-m picks the module (default shell; see wdp module for the full list); -a passes arguments:
tokens containing = are treated as k=v parameters, the rest are joined into the free-form command;
-b escalates privileges (become)
Dry run: --check is zero-risk probing, --diff a content-level comparison (same semantics as run)
--format renders per-host output through a Go template (pipe-friendly), with fields
.host/.stdout/.stderr/.rc/.changed/.failed/.msg; it silences all other output

Host sources (pick one): --hosts inline specs (IP/host[:port], ranges like 10.8.2.101-104; saves an
inventory file for temporary targets — the host pattern may then be omitted and defaults to all) or
-i inventory files + [host-pattern] (same syntax as playbook hosts: comma union, ! prefix exclusion,
:& intersection chains, path.Match wildcards — matched against both group and host names, group hits
expand to their members, subgroups recurse)

Examples:
wdp adhoc -m shell -a 'uptime' webservers
wdp adhoc -m shell -a 'uptime' --hosts 10.8.2.101-104
wdp adhoc -m package -a 'name=nginx state=present' 'all,!canary'
wdp adhoc -m command -a 'df -h /' all --format '{{.host}}: {{.stdout}}'
`, `跨主机一次性执行单个模块（免写 playbook）

-m 指定模块（默认 shell；wdp module 查看全部）；-a 传参：含 = 的 token 视为
k=v 参数，其余拼接为 free-form；-b 提权执行（become）
预演：--check 零风险探测、--diff 内容级对照（语义与 run 相同）
--format 用 Go 模板逐主机格式化输出（脚本管道友好），可用字段
.host/.stdout/.stderr/.rc/.changed/.failed/.msg；指定后静默其余呈现

主机来源二选一：--hosts 内联表达式（IP/主机[:port]，支持 10.8.2.101-104
区间；临时目标免 inventory 文件，此时主机模式可省略、默认 all）或
-i inventory 清单 + [host-pattern]（语法同 playbook hosts：逗号联合、
! 前缀排除、:& 交集链、path.Match 通配——同时匹配组名与主机名，组命中
展开为成员，子组递归）

示例：
wdp adhoc -m shell -a 'uptime' webservers
wdp adhoc -m shell -a 'uptime' --hosts 10.8.2.101-104
wdp adhoc -m package -a 'name=nginx state=present' 'all,!canary'
wdp adhoc -m command -a 'df -h /' all --format '{{.host}}: {{.stdout}}'
`)
}

// newAdhocCmd 构造 `wdp adhoc`。
func newAdhocCmd() *cobra.Command {
	opts := adhocOptions{}
	cmd := &cobra.Command{
		Use:   "adhoc -m shell -a 'uptime' [host-pattern]",
		Short: i18n.T("one-off single-module execution", "跨主机一次性执行单个模块"),
		Long:  adhocHelp(),
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := "all"
			if len(args) == 1 {
				pattern = args[0]
			}
			return runAdhoc(cmd.Context(), pattern, opts)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&opts.mod, "module", "m", "shell", "module name")
	f.StringVarP(&opts.argStr, "args", "a", "", i18n.T(
		"module args (free-form or k=v list)",
		"模块参数（free-form 或 k=v 列表）"))
	f.BoolVarP(&opts.become, "become", "b", false, "escalate privileges")
	f.StringSliceVar(&opts.hostsInline, "hosts", nil, i18n.T(
		"inline host specs instead of an inventory file (IP/host[:port], ranges like 10.8.2.101-104); host pattern then defaults to all",
		"内联主机表达式，替代 inventory 清单文件（IP/主机[:port]，支持 10.8.2.101-104 区间）；此时主机模式默认 all"))
	f.StringVar(&opts.format, "format", "", i18n.T(
		"format per-host output with a Go template, e.g. '{{.host}}: {{.stdout}}' (fields: .stdout/.stderr/.rc/.changed/.failed/.msg)",
		"用 Go 模板逐主机格式化输出，如 '{{.host}}: {{.stdout}}'（可用字段：.stdout/.stderr/.rc/.changed/.failed/.msg）"))
	f.BoolVar(&opts.check, "check", false, "check mode: dry-run without applying changes")
	f.BoolVar(&opts.diff, "diff", false, "diff mode: content-level diff with --check")
	f.BoolVar(&opts.failFast, "fail-fast", false, i18n.T(
		"abort remaining batches and plays as soon as any host fails (in-flight hosts finish first)",
		"任一主机失败即中止后续批次与 play（在途主机先跑完）"))
	return cmd
}

type adhocOptions struct {
	mod         string
	argStr      string
	format      string
	become      bool
	check       bool
	diff        bool
	failFast    bool
	hostsInline []string
}

// runAdhoc 把单模块装配为单 play 单任务交给 executor 执行。
func runAdhoc(ctx context.Context, pattern string, opts adhocOptions) error {
	if _, ok := module.Get(opts.mod); !ok {
		return fmt.Errorf("unknown module %q (run `wdp module` for the list)", opts.mod)
	}
	// 主机来源：--hosts 内联表达式（临时目标/一次性排查，免清单文件）或
	// -i 清单（口径与 run --hosts 一致）
	inv, _, err := hostSource(opts.hostsInline)
	if err != nil {
		return err
	}
	free, margs := parseAdhocArgs(opts.argStr)
	opts.check = normalizeDiffFlag(opts.diff, opts.check)
	play := &model.Play{
		Hosts: pattern, Become: opts.become,
		Tasks: []*model.Task{{Module: opts.mod, FreeForm: free, Args: margs, Become: &opts.become}},
	}
	// 零主机即失败（与 run 同源同口径）：模式合法但命中空集时，此前会跑完
	// 空 RECAP 并退出 0，CI 把"什么都没做"当成执行成功。
	if hosts := inv.SelectPlays([]*model.Play{play}, ""); len(hosts) == 0 {
		return noHostsError("", pattern)
	}
	rep, finish := buildReporter()
	if opts.format != "" {
		// --format：逐主机模板化输出（脚本管道友好），静默其余呈现
		rep = report.NewFormatter(os.Stdout, opts.format)
		finish = func() error { return nil }
	}
	conns := conn.NewManagerWithDefaults(connDefaults())
	conns.SetConnectConcurrency(2 * config.Current().Forks())
	ex := executor.New(inv, conns, rep, executor.Options{
		Forks:            config.Current().Forks(),
		TaskTimeout:      config.Current().Run.TaskTimeout,
		CheckMode:        opts.check,
		DiffMode:         opts.diff,
		FailFast:         opts.failFast,
		MaxDownloadBytes: maxDownloadBytes(),
		MaxUploadBytes:   maxUploadBytes(),
	})
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	failed := ex.Run(ctx, []*model.Play{play})
	conns.CloseAll()
	if werr := finish(); werr != nil {
		return werr
	}
	if failed {
		return errPlayFailed
	}
	return nil
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
