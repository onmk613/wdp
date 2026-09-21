package cli

// `wdp drift`：跨主机漂移巡检（P1a 最小闭环——检测与报告，不做自动收敛）。
//
// 检测与分类内核在 internal/drift（marker 读取 play、逐主机比对），主机
// 选取用 inventory.SelectLimited（与 run/executor 同口径）；本文件只做
// 命令装配与结论呈现。退出码：存在 DRIFTED / UNREACHABLE 时非零（可直接
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
	"wdp/internal/model"
	"wdp/internal/report"

	"github.com/spf13/cobra"
)

const driftHelp = `
跨主机配置漂移巡检（只读检测与报告，不自动收敛）

读取各主机 release marker，与当前 chart + values 的摘要逐主机比对分类：
OK 一致 / DRIFTED values 已变（改配置未部署或有人手改现场；marker v2 附字段级差异）/
OUTDATED 部署的是旧 chart 版本 / NOT-DEPLOYED 无 marker / UNREACHABLE 连接失败
判失败口径：DRIFTED / UNREACHABLE 非零退出（可直接进 CI）；
NOT-DEPLOYED / OUTDATED 只列出不判失败（扩容中与待升级是常态，不是漂移）
收敛口径：重跑 wdp run <chart> 幂等重部署
可选位置参数限定主机模式（默认 all）；--limit 进一步收窄；-f/--set 提供巡检口径的 values
要求 chart 未禁用 release marker（no_marker: true 报错）

示例：
wdp drift ./myapp -f envs/prod.yaml
wdp drift ./myapp 'webservers:!canary'
`

// newDriftCmd 构造 `wdp drift`。
func newDriftCmd() *cobra.Command {
	var (
		limit                string
		valuesFiles, setArgs []string
	)
	cmd := &cobra.Command{
		Use:   "drift <chart-dir|chart.tgz> [host-pattern]",
		Short: "compare each host's release marker against the current chart values (read-only)",
		Long:  driftHelp,
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := "all"
			if len(args) == 2 {
				pattern = args[1]
			}
			return runDrift(cmd.Context(), args[0], pattern, limit, valuesFiles, setArgs)
		},
	}
	f := cmd.Flags()
	f.StringVar(&limit, "limit", "", "further limit hosts (group/host/!exclude)")
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
	finish()

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
