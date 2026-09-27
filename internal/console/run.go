package console

// 应用执行的领域编排：加载 chart → 相位 plays → values 来源推导
//（chart values / 主机 release marker）→ hosts 模式回退判定 → executor
// 执行。进度上报经 report.Reporter 接口注入（web 的 dbReporter 落库 +
// SSE），传输侧的 mTLS scheme 探测留 web。

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"wdp/internal/chart"
	"wdp/internal/config"
	"wdp/internal/conn"
	"wdp/internal/conn/agentc"
	"wdp/internal/executor"
	"wdp/internal/inventory"
	"wdp/internal/markerread"
	"wdp/internal/model"
	"wdp/internal/report"
	"wdp/internal/store"
)

// RunService 应用执行领域服务。
type RunService struct {
	Store  *store.Store
	Logger *slog.Logger
}

// RunOneApp 加载 chart 并执行到给定主机集合。接线与 CLI 的 run 一致：
// Open（合并 values + 用 CollectHelpers 构建模板引擎）→ 相位 plays →
// 按相位取 values 来源（chart values / 主机 release marker）→ executor。
func (r *RunService) RunOneApp(ctx context.Context, tgz string, hosts []*model.Host, phase string, rep report.Reporter) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	// OpenWithLimits 而非 LoadWithLimits：前者额外做 BuildValues 与
	// render.NewEngine(ch.CollectHelpers())——少这一步，chart 内
	// {{ include "xxx.helper" . }} 与 values 引用全部失效
	ch, values, eng, err := chart.OpenWithLimits(tgz, nil, nil, chart.Limits{})
	if err != nil {
		return fmt.Errorf("load chart: %w", err)
	}
	defer ch.Close()

	plays, err := ch.PhasePlays(phase)
	if err != nil {
		return fmt.Errorf("phase %q: %w", phase, err)
	}
	if len(plays) == 0 {
		return fmt.Errorf("phase %q has no plays", phase)
	}
	spec := ch.PhaseSpecFor(phase)

	// 并发与任务超时从 wdp.cfg 取值（server 经 CLI 组合根启动，配置已
	// 装载）；缺省回退保持既有硬编码口径：forks 归一方法内置默认 5，
	// task_timeout 未配置（<=0）时回退 600
	taskTimeout := config.Current().Run.TaskTimeout
	if taskTimeout <= 0 {
		taskTimeout = 600
	}
	opts := executor.Options{
		Forks: config.Current().Forks(), BaseDir: ch.Dir, Phase: phase, WdpVersion: "server",
		TaskTimeout: taskTimeout,
		Chart:       ch, Engine: eng,
	}
	// values 来源按相位推导（与 CLI 同规则）：部署类相位用 chart values
	//（过 required + schema 校验）；清除 marker 的相位（uninstall 等）用
	// 各主机 marker 记录的实际部署入参——缺失即拒绝执行，绝不静默回退
	// values.yaml（卸载会作用在错误路径上）
	switch spec.EffectiveValuesFrom() {
	case chart.ValuesFromMarker:
		hostValues, verr := r.MarkerValues(ctx, ch, hosts, spec.Destructive())
		if verr != nil {
			return verr
		}
		opts.HostValues = hostValues
		// 约束：map 迭代随机——多主机 marker values 不一致时选哪台必须
		// 确定（按主机名排序取首个），否则 run 级 values 每次执行都不同
		if v := FirstHostValues(hostValues); v != nil {
			values = v
		}
	default:
		if spec.Release {
			if err := ch.ValidateRequired(values); err != nil {
				return err
			}
			if err := ch.ValidateValuesSchema(values); err != nil {
				return err
			}
			if err := ch.ValidateSubchartsSchema(values); err != nil {
				return err
			}
		}
	}
	opts.Values = values

	inv := inventory.FromHosts(hosts)
	// hosts 语义（web）：裸任务/未写 hosts 的 play 打选择器全集（空→
	// executor 按 all）；显式 hosts 的 play 在选择器范围内按台账组细分
	//（多组应用：选择器圈大组，组内 web/db 分工）。兼容：模式里的组/
	// 主机名一个都不认识（旧 chart 的 hosts: <应用名> 写法——台账并无
	// 此组）时回退全集并告警，避免老 chart 在 web 变成空跑；组存在但
	// 选择器没圈到成员则保持原样（该 play 空跑跳过——按需局部的语义）
	known := r.KnownTargetNames()
	for _, p := range plays {
		if p.Hosts == "" {
			p.Hosts = "all"
			continue
		}
		if unknown, ok := UnknownHostPattern(p.Hosts, known); ok {
			r.Logger.Warn("play hosts pattern matched no known group/host, falling back to full selection",
				"pattern", p.Hosts, "unknown", unknown, "hosts", len(hosts))
			p.Hosts = "all"
		}
	}
	// 组存在但选择器范围内无成员的 play 跳过（按需局部执行的语义：
	// 只圈了 db 的选择器跑 web+db 应用，web play 空跑）——而不是让
	// executor 的选择报错把整个 run 判失败
	// 原地过滤覆盖 PhasePlays 返回的内部切片——当前每次 run 用新 chart 故无害，
	// 复用 chart 缓存前必须改为新切片。
	runnable := plays[:0]
	for _, p := range plays {
		sel, serr := inv.Select(p.Hosts)
		if serr != nil || len(sel) == 0 {
			r.Logger.Info("play skipped: hosts matched nothing in selection", "play", p.Name, "hosts", p.Hosts)
			continue
		}
		runnable = append(runnable, p)
	}
	if len(runnable) == 0 {
		return fmt.Errorf("selection has no hosts for any play of phase %q", phase)
	}
	plays = runnable

	conns := conn.NewManagerWithDefaults(&conn.Defaults{Conn: "agent"})
	// 连接并发与 CLI 同口径 2×forks（缺省 5 → 10）
	conns.SetConnectConcurrency(2 * config.Current().Forks())
	ex := executor.New(inv, conns, rep, opts)
	failed := ex.Run(ctx, plays)
	if failed {
		return fmt.Errorf("run finished with failed hosts (see run tasks)")
	}
	return nil
}

// MarkerValues 读取各主机 release marker 还原实际部署 values（与 CLI 的
// resolveMarkerValues 同语义）：marker 缺失/v1/不可达都报错并列出主机名。
// 读取、三分类与聚合校验核心收敛于 markerread（与 CLI 共享），此处只保留
// console 侧的合并报错文案。并发与单主机超时同 RunOneApp 从 wdp.cfg 取值
// （并发 2×forks、task_timeout 未配置回退 30s）；结果按 hosts 序回放
// 聚合——并发不引入结果漂移。
func (r *RunService) MarkerValues(ctx context.Context, ch *chart.Chart, hosts []*model.Host, validate bool) (map[string]map[string]any, error) {
	dc := &conn.Defaults{Conn: "agent"}
	// 单主机超时取 [run].task_timeout；未配置（<=0，含"不限"）对单次
	// marker 读取无意义，回退 30s——串行版最坏 N×30s，上百台主机的
	// uninstall 相位会拖到小时级
	perHost := 30 * time.Second
	if t := time.Duration(config.Current().Run.TaskTimeout) * time.Second; t > 0 {
		perHost = t
	}
	results := markerread.Read(ctx, ch.MarkerPath(), hosts,
		func(ctx context.Context, h *model.Host) (conn.Conn, func(), error) {
			// 直拨 agentc：不建 Manager，单主机读毕即关
			ac := agentc.New(h, dc)
			return ac, func() { _ = ac.Close() }, nil
		},
		// 并发与 RunOneApp 的连接并发同口径 2×forks（forks 缺省 5 → 10）
		markerread.Options{Concurrency: 2 * config.Current().Forks(), PerHostTimeout: perHost})

	out := make(map[string]map[string]any, len(hosts))
	var missing, legacy, failed []string
	for i, res := range results {
		name := hosts[i].Name
		switch res.Kind {
		case markerread.KindOK:
			out[name] = res.Marker.Values
		case markerread.KindMissing:
			missing = append(missing, name)
		case markerread.KindLegacy:
			legacy = append(legacy, name)
		default:
			// failed 明细按 console 侧原文案：执行错误原样、退出码带
			// stderr（截断 120）、解析失败注明 marker unreadable
			var d string
			switch {
			case res.ParseErr != nil:
				d = "marker unreadable: " + res.ParseErr.Error()
			case res.Err != nil:
				d = res.Err.Error()
			default:
				d = fmt.Sprintf("exit %d: %s", res.Code, Truncate(strings.TrimSpace(res.Stderr), 120))
			}
			failed = append(failed, name+" ("+d+")")
		}
	}
	if len(missing) > 0 || len(legacy) > 0 || len(failed) > 0 {
		var parts []string
		if len(missing) > 0 {
			parts = append(parts, "no release marker: "+strings.Join(missing, ", "))
		}
		if len(legacy) > 0 {
			parts = append(parts, "marker without values (deployed by an older wdp): "+strings.Join(legacy, ", "))
		}
		if len(failed) > 0 {
			parts = append(parts, "unreadable: "+strings.Join(failed, ", "))
		}
		return nil, fmt.Errorf("this phase restores values from the release marker, but: %s (deploy first, or run uninstall from the CLI with explicit values)",
			strings.Join(parts, "; "))
	}
	if validate {
		// 聚合校验（与 CLI 同口径）：相同 values 只校验一次、归因与序列化
		// 失败主机按主机名字典序稳定（此前逐主机全量校验、报错主机随
		// map 迭代序漂移）
		if err := markerread.Validate(ch, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// KnownTargetNames 台账里全部组名与主机名（web 执行的 hosts 模式兼容
// 判定用：模式里的段一个都不认识 → 旧 chart 写法，回退全集）。
func (r *RunService) KnownTargetNames() map[string]bool {
	out := map[string]bool{}
	if gs, err := r.Store.ListGroups(); err == nil {
		for _, g := range gs {
			out[g.Name] = true
		}
	}
	if hs, err := r.Store.ListHosts(""); err == nil {
		for _, h := range hs {
			out[h.Name] = true
		}
	}
	return out
}

// UnknownHostPattern 解析 hosts 模式的正例段，返回第一个既非已知组也非
// 已知主机名的段（通配段视为已知）。ok=false 表示全部段都认识。
func UnknownHostPattern(pattern string, known map[string]bool) (string, bool) {
	for token := range strings.SplitSeq(pattern, ",") {
		token = strings.TrimSpace(token)
		if token == "" || strings.HasPrefix(token, "!") {
			continue // 排除段：未知名只会让它匹配不到，不改变正例范围
		}
		for seg := range strings.SplitSeq(token, ":&") {
			seg = strings.TrimSpace(seg)
			if seg == "" || seg == "all" || seg == "*" ||
				strings.ContainsAny(seg, "*?[") || known[seg] {
				continue
			}
			return seg, true
		}
	}
	return "", false
}

// FirstHostValues 多主机 marker values 不一致时按主机名排序取首个
// （确定性：run 级 values 不随 map 迭代顺序漂移；实现收敛于
// markerread.FirstValues，与 CLI 共享同一口径）。
func FirstHostValues(hostValues map[string]map[string]any) map[string]any {
	return markerread.FirstValues(hostValues)
}

// Truncate 截断长文本（marker 错误信息等入 message 用）。
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
