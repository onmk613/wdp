package cli

// `wdp drift`：跨主机漂移巡检（P1a 最小闭环——检测与报告，不做自动收敛）。
//
// 检测与分类内核在 internal/drift（marker 读取 play、逐主机比对），主机
// 选取用 inventory.SelectLimited（与 run/executor 同口径）；本文件只做
// 命令装配与结论呈现。退出码：存在 DRIFTED / UNREACHABLE / FAILED 时非零（可直接
// 进 CI）。

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"wdp/internal/chart"
	"wdp/internal/config"
	"wdp/internal/conn"
	"wdp/internal/drift"
	"wdp/internal/executor"
	"wdp/internal/i18n"
	"wdp/internal/model"
	"wdp/internal/report"

	"github.com/spf13/cobra"
)

// driftHelp 返回 `wdp drift` 的长帮助（调用时求值）。
func driftHelp() string {
	return i18n.T(`Cross-host configuration drift inspection (read-only detection and reporting; never auto-converges)

Reads each host's release marker and classifies it per host against the digest of the current chart + values:
OK identical / DRIFTED values changed (config edited without deploying, or someone changed the host by
hand; marker v2 adds field-level differences) / OUTDATED an older chart version is deployed / NOT-DEPLOYED
no marker / UNREACHABLE connection failed / FAILED task execution failed (become denied etc., distinguished
from connection problems)
Failure criteria: DRIFTED / UNREACHABLE / FAILED exit non-zero (CI-ready); NOT-DEPLOYED / OUTDATED are only
listed and do not count as failures (scaling out and pending upgrades are normal, not drift)
Convergence: rerun wdp run <chart> for an idempotent redeploy
An optional positional argument limits the host pattern (default all); --limit narrows it further;
-f/--set supplies the values for the inspection
Requires a chart with release markers enabled (no_marker: true is an error)

Examples:
wdp drift ./myapp -f envs/prod.yaml
wdp drift ./myapp 'webservers:!canary'
`, `跨主机配置漂移巡检（只读检测与报告，不自动收敛）

读取各主机 release marker，与当前 chart + values 的摘要逐主机比对分类：
OK 一致 / DRIFTED values 已变（改配置未部署或有人手改现场；marker v2 附字段级差异）/
OUTDATED 部署的是旧 chart 版本 / NOT-DEPLOYED 无 marker / UNREACHABLE 连接失败 /
FAILED 任务执行失败（become 被拒等，与连接问题区分）
判失败口径：DRIFTED / UNREACHABLE / FAILED 非零退出（可直接进 CI）；
NOT-DEPLOYED / OUTDATED 只列出不判失败（扩容中与待升级是常态，不是漂移）
收敛口径：重跑 wdp run <chart> 幂等重部署
可选位置参数限定主机模式（默认 all）；--limit 进一步收窄；-f/--set 提供巡检口径的 values
要求 chart 未禁用 release marker（no_marker: true 报错）

示例：
wdp drift ./myapp -f envs/prod.yaml
wdp drift ./myapp 'webservers:!canary'
`)
}

// newDriftCmd 构造 `wdp drift`。
func newDriftCmd() *cobra.Command {
	var (
		limit                string
		valuesFiles, setArgs []string
	)
	cmd := &cobra.Command{
		Use: "drift <chart-dir|chart.tgz> [host-pattern]",
		Short: i18n.T("compare each host's release marker against the current chart values (read-only)",
			"逐主机比对 release marker 与当前 chart values（只读）"),
		Long: driftHelp(),
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := "all"
			if len(args) == 2 {
				pattern = args[1]
			}
			return runDrift(cmd.Context(), args[0], pattern, limit, valuesFiles, setArgs)
		},
	}
	f := cmd.Flags()
	f.StringVar(&limit, "limit", "", i18n.T(
		"further limit hosts (group/host/!exclude)",
		"进一步收窄主机（组/主机/!排除）"))
	chartValueFlags(cmd, &valuesFiles, &setArgs)
	return cmd
}

// runDrift 执行巡检并打印结论。
func runDrift(ctx context.Context, target, pattern, limit string, valuesFiles, setArgs []string) error {
	ch, values, _, err := chart.OpenWithLimits(target, valuesFiles, setArgs,
		chart.Limits{MaxExtractBytes: config.Current().MaxExtractBytes()})
	if err != nil {
		return err
	}
	defer ch.Close()
	if !ch.MarkerEnabled() {
		return fmt.Errorf("chart %s disables release markers (no_marker: true), drift cannot work", ch.Meta.Name)
	}

	inv, err := loadInventories()
	if err != nil {
		return err
	}
	hosts, err := inv.SelectLimited(pattern, limit)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts selected by pattern %q", pattern)
	}

	// 同名碰撞预检（与 run 同口径）：drift 的摘要口径是 chart values，
	// inventory_override 覆盖不进摘要——被遮蔽变量照旧告警，提醒口径差。
	reportValueCollisions(os.Stderr, values, hosts, ch.Meta.InventoryOverride)

	rep, finish := buildReporter()
	capture := drift.NewCapture(rep)
	conns := conn.NewManagerWithDefaults(connDefaults())
	conns.SetConnectConcurrency(2 * config.Current().Forks())
	ex := executor.New(inv, conns, capture, executor.Options{
		Forks:       config.Current().Forks(),
		Limit:       limit,
		TaskTimeout: config.Current().Run.TaskTimeout,

		MaxDownloadBytes: maxDownloadBytes(),
		MaxUploadBytes:   maxUploadBytes(),
	})
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	_ = ex.Run(ctx, []*model.Play{drift.ReadMarkerPlay(ch.Meta.Name, ch.MarkerPath(), pattern)})
	conns.CloseAll()

	// 汇总：逐主机比对 marker 与当前 values 摘要（敏感键口径与 marker 写入
	// 一致；marker v2 时 DRIFTED 附字段级差异）
	wantSHA := ch.ValuesDigestOf(values)
	rows, failed := drift.Classify(hosts, capture.Results(), ch.Meta.Version, wantSHA, values)

	// 逐主机结论：JSON 模式必须让 stdout 只含最终 JSON 文档（人类可读摘要
	// 改走 stderr，同时把分类行进 JSON 的 drift 字段供 CI 解析）。附加字段
	// 必须在 finish 之前写入——JSON 文档在 Finish 时一次性输出。
	out := io.Writer(os.Stdout)
	if gOutput == "json" {
		out = os.Stderr
		if js, ok := rep.(*report.JSONReporter); ok {
			js.SetExtra("drift", rows)
		}
	}
	// 写失败（破管道等）优先于漂移结论上报：CI 拿到 0 字节 JSON 时，错误
	// 码必须说明"报告没写出来"，而不是把漂移结论静默吞掉
	if werr := finish(); werr != nil {
		return werr
	}

	fmt.Fprintf(out, "chart %s %s  expected values sha %s\n", ch.Meta.Name, ch.Meta.Version, wantSHA)
	for _, r := range rows {
		fmt.Fprintf(out, "  %-28s %-13s %s\n", r.Host, r.State, r.Detail)
	}
	if failed > 0 {
		return fmt.Errorf("drift detected on %d host(s): rerun `wdp run %s` to reconcile (read-only check, nothing was changed)", failed, target)
	}
	fmt.Fprintln(out, "no values drift detected")
	return nil
}
