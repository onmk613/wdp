package cli

// 非部署相位的 values 来源：主机 marker（§5.1 新语义）。
//
// 部署相位（deploy 及声明 release 的相位）从 values.yaml + -f + --set 解析；
// 其他相位（uninstall / status / 自定义）默认读取各主机 marker 中记录的
// 实际部署 values，-f/--set 降级为对 marker values 的显式覆盖。marker 缺失
// 或为 v1（无 values）时报错——绝不静默回退到 values.yaml 默认值，那会让
// 卸载任务作用在错误路径上并报告成功。
//
// 读取、missing/legacy/failed 三分类与聚合校验的核心收敛于 markerread
//（与 console 共享同一实现）；本文件只保留 CLI 侧的错误文案排版与
// -f/--set 覆盖合并。

import (
	"context"
	"fmt"
	"os"
	"strings"

	"wdp/internal/chart"
	"wdp/internal/config"
	"wdp/internal/conn"
	"wdp/internal/markerread"
	"wdp/internal/model"
)

// resolveMarkerValues 读取 hosts 的 release marker 并还原各主机实际生效的
// values（marker values 基线 + 显式 -f/--set 覆盖）。返回主机名 → values。
// 任一主机 marker 缺失 / v1 / 不可达都报错并列出主机——宁可拒绝执行，
// 不静默用错误入参跑 destructive 相位。validate 控制 required + schema
// 校验（清除 marker 的相位与部署同门控，纯只读相位不校验）。
func resolveMarkerValues(ctx context.Context, ch *chart.Chart, hosts []*model.Host, valuesFiles, setArgs []string, validate bool) (map[string]map[string]any, error) {
	if len(valuesFiles) > 0 || len(setArgs) > 0 {
		fmt.Fprintln(os.Stderr, "[values] explicit -f/--set override the values recorded in the release marker")
	}
	markers, err := readHostMarkers(ctx, ch, hosts)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]any, len(markers))
	for host, mk := range markers {
		merged, err := chart.ApplyOverrides(mk.Values, valuesFiles, setArgs)
		if err != nil {
			return nil, fmt.Errorf("host %s: %w", host, err)
		}
		out[host] = merged
	}
	// 校验（required + schema + 子 chart 走查）：清除 marker 的相位
	//（uninstall 等）用 values 拼删除路径，非法入参必须在执行前拦截。
	// 相同 values 只校验一次、归因与序列化失败主机均按主机名字典序稳定
	//（markerread.Validate；此前 map 迭代序随机且 json.Marshal 错误被吞）。
	if !validate {
		return out, nil
	}
	if err := markerread.Validate(ch, out); err != nil {
		return nil, err
	}
	return out, nil
}

// readHostMarkers 读取各主机 marker（并发受 forks 约束；become root——
// marker 0600 含 values 内容）。marker 缺失 / v1 / 不可达分别汇总报错；
// 读取与分类走 markerread（与 console 共享），清单按 hosts 声明序——
// 此前为并发完成序，报错主机次序随机漂移。
func readHostMarkers(ctx context.Context, ch *chart.Chart, hosts []*model.Host) (map[string]*chart.Marker, error) {
	conns := conn.NewManagerWithDefaults(connDefaults())
	conns.SetConnectConcurrency(2 * config.Current().Forks())
	defer conns.CloseAll()

	results := markerread.Read(ctx, ch.MarkerPath(), hosts,
		func(ctx context.Context, h *model.Host) (conn.Conn, func(), error) {
			// 连接由 Manager 跨主机复用（自愈），统一 CloseAll 收尾，
			// 单主机读毕不关
			c, err := conns.Get(ctx, h)
			return c, nil, err
		},
		markerread.Options{Concurrency: max(2*config.Current().Forks(), 1)})

	out := make(map[string]*chart.Marker, len(hosts))
	var missing, legacy, failed []string
	for i, res := range results {
		name := hosts[i].Name
		switch res.Kind {
		case markerread.KindOK:
			out[name] = res.Marker
		case markerread.KindMissing:
			missing = append(missing, name)
		case markerread.KindLegacy:
			legacy = append(legacy, name)
		default:
			failed = append(failed, fmt.Sprintf("%s (%v)", name, failureDetail(res)))
		}
	}

	switch {
	case len(missing) > 0:
		return nil, fmt.Errorf("no release marker on host(s) %s — nothing recorded to uninstall from; "+
			"deploy first, or narrow the target with --limit", strings.Join(missing, ", "))
	case len(legacy) > 0:
		return nil, fmt.Errorf("release marker on host(s) %s was written by an older wdp and carries no resolved values; "+
			"run a deploy once to upgrade the marker", strings.Join(legacy, ", "))
	case len(failed) > 0:
		return nil, fmt.Errorf("failed to read release marker: %s", strings.Join(failed, "; "))
	}
	return out, nil
}

// failureDetail 还原 CLI 版 failed 类明细文案：执行错误原样、退出码非零
// 取 stderr 首行（无则 exit N）、解析失败注明 marker unreadable。
func failureDetail(res markerread.HostResult) any {
	switch {
	case res.ParseErr != nil:
		return fmt.Sprintf("marker unreadable: %v", res.ParseErr)
	case res.Err != nil:
		return res.Err
	default:
		return errOrCode(conn.ExecResult{Code: res.Code, Stderr: res.Stderr})
	}
}

// errOrCode 归因非零退出的主机侧错误：stderr 首行（多行噪声只留一行），
// 无 stderr 时退回退出码。
func errOrCode(res conn.ExecResult) any {
	if res.Stderr != "" {
		return strings.TrimSpace(strings.SplitN(res.Stderr, "\n", 2)[0])
	}
	return fmt.Sprintf("exit %d", res.Code)
}
