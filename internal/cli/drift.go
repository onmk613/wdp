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
	"os"
	"os/signal"
	"syscall"

	"wdp/internal/chart"
	"wdp/internal/config"
	"wdp/internal/conn"
	"wdp/internal/drift"
	"wdp/internal/executor"
	"wdp/internal/model"

	"github.com/spf13/cobra"
)

// newDriftCmd 构造 `wdp drift`。
func newDriftCmd() *cobra.Command {
	var (
		limit                string
		valuesFiles, setArgs []string
	)
	cmd := &cobra.Command{
		Use:   "drift <chart-dir|chart.tgz> [host-pattern]",
		Short: "compare each host's release marker against the current chart values (read-only)",
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
	finish()

	// 汇总：逐主机比对 marker 与当前 values 摘要
	wantSHA := chart.ValuesDigest(values)
	rows, failed := drift.Classify(hosts, capture.Results(), ch.Meta.Version, wantSHA)

	out := os.Stdout
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
