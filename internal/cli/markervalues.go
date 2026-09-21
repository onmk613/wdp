package cli

// 非部署相位的 values 来源：主机 marker（§5.1 新语义）。
//
// 部署相位（deploy 及声明 release 的相位）从 values.yaml + -f + --set 解析；
// 其他相位（uninstall / status / 自定义）默认读取各主机 marker 中记录的
// 实际部署 values，-f/--set 降级为对 marker values 的显式覆盖。marker 缺失
// 或为 v1（无 values）时报错——绝不静默回退到 values.yaml 默认值，那会让
// 卸载任务作用在错误路径上并报告成功。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"wdp/internal/chart"
	"wdp/internal/config"
	"wdp/internal/conn"
	"wdp/internal/model"
	"wdp/internal/shellquote"
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
	// 相同 values 只校验一次（多主机典型情形）。
	if !validate {
		return out, nil
	}
	seen := map[string]bool{}
	for host, v := range out {
		key, _ := json.Marshal(v)
		if seen[string(key)] {
			continue
		}
		seen[string(key)] = true
		if err := validatePhaseValues(ch, v, host); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// validatePhaseValues 按部署相位同等强度校验一份 values（错误带上主机名）。
func validatePhaseValues(ch *chart.Chart, values map[string]any, host string) error {
	if err := ch.ValidateRequired(values); err != nil {
		return fmt.Errorf("host %s: %w", host, err)
	}
	if err := ch.ValidateValuesSchema(values); err != nil {
		return fmt.Errorf("host %s: %w", host, err)
	}
	if err := ch.ValidateSubchartsSchema(values); err != nil {
		return fmt.Errorf("host %s: %w", host, err)
	}
	return nil
}

// readHostMarkers 读取各主机 marker（并发受 forks 约束；become root——
// marker 0600 含 values 内容）。marker 缺失 / v1 / 不可达分别汇总报错。
func readHostMarkers(ctx context.Context, ch *chart.Chart, hosts []*model.Host) (map[string]*chart.Marker, error) {
	conns := conn.NewManagerWithDefaults(connDefaults())
	conns.SetConnectConcurrency(2 * config.Current().Forks())
	defer conns.CloseAll()

	script := fmt.Sprintf("cat -- %s 2>/dev/null || echo __MISSING__", shellquote.Quote(ch.MarkerPath()))

	var mu sync.Mutex
	out := make(map[string]*chart.Marker, len(hosts))
	var missing, legacy, failed []string

	sem := make(chan struct{}, max(2*config.Current().Forks(), 1))
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(h *model.Host) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cn, err := conns.Get(ctx, h)
			if err != nil {
				mu.Lock()
				failed = append(failed, fmt.Sprintf("%s (%v)", h.Name, err))
				mu.Unlock()
				return
			}
			res, err := cn.Exec(ctx, conn.ExecRequest{Script: script, BecomeUser: "root", TimeoutMs: 30_000})
			if err != nil || res.Code != 0 {
				mu.Lock()
				failed = append(failed, fmt.Sprintf("%s (%v)", h.Name, errOrCode(err, res)))
				mu.Unlock()
				return
			}
			stdout := strings.TrimSpace(res.Stdout)
			if stdout == "__MISSING__" || stdout == "" {
				mu.Lock()
				missing = append(missing, h.Name)
				mu.Unlock()
				return
			}
			mk, perr := chart.ParseMarker([]byte(stdout))
			if perr != nil {
				mu.Lock()
				failed = append(failed, fmt.Sprintf("%s (marker unreadable: %v)", h.Name, perr))
				mu.Unlock()
				return
			}
			if mk.Schema() < chart.MarkerSchemaV2 {
				mu.Lock()
				legacy = append(legacy, h.Name)
				mu.Unlock()
				return
			}
			mu.Lock()
			out[h.Name] = mk
			mu.Unlock()
		}(h)
	}
	wg.Wait()

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

func errOrCode(err error, res conn.ExecResult) any {
	if err != nil {
		return err
	}
	if res.Stderr != "" {
		return strings.TrimSpace(strings.SplitN(res.Stderr, "\n", 2)[0])
	}
	return fmt.Sprintf("exit %d", res.Code)
}
