// Package markerread 收敛 CLI 与 console 此前两份重复（且已行为漂移）的
// "读主机 release marker → 三分类 → 聚合校验"核心：同一句 cat 哨兵脚本、
// missing/legacy/failed 三分类、按 values 去重后校验。
//
// 依赖方向：本包只依赖 conn / chart / model / shellquote，不 import
// cli / console（二者向本包收敛，无环）。调用方的差异经参数注入——
// 连接策略（CLI 走 conn.Manager 复用自愈、console 直拨 agentc）、并发度、
// 单主机超时；错误文案排版与返回结构留在各自包内，共享的只是口径。
package markerread

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"wdp/internal/chart"
	"wdp/internal/conn"
	"wdp/internal/model"
	"wdp/internal/shellquote"
)

// Kind 单主机 marker 读取结果的分类。
type Kind int

const (
	KindOK      Kind = iota // v2 marker 且携带 resolved values
	KindMissing             // 主机无 marker（脚本回显哨兵或输出为空）
	KindLegacy              // v1 marker（或 v2 缺 values 字段）：无可还原入参
	KindFailed              // 不可达 / 脚本非零退出 / marker 内容不可解析
)

// HostResult 单主机读取结果。Read 按 hosts 声明序返回，聚合格式由调用方
// 自定（CLI 的分类别报错与 console 的合并报错都在各自包内拼装）。
type HostResult struct {
	Kind   Kind
	Marker *chart.Marker // KindOK 时非 nil

	// KindFailed 的三种成因恰好设其一（分类即归因，调用方按各自文案
	// 格式化——两调用方对退出码/stderr 的呈现口径不同）：
	Err      error // 连接建立或执行出错（含超时/取消）
	Code     int   // 脚本退出码非零（Stderr 携带原始输出）
	Stderr   string
	ParseErr error // marker 内容解析失败
}

// Options 读取参数：两调用方的既有分歧在此参数化，各取原值。
type Options struct {
	Concurrency    int           // 主机级并发上限（信号量）：CLI 为 2×forks，console 固定 10
	PerHostTimeout time.Duration // 单主机 Exec 的 ctx 超时：console 30s；CLI 0（不限，由请求 timeout_ms 约束）
}

// ConnFactory 构造单主机连接。release 释放该连接的资源（该主机读取结束
// 后由共享核心调用，可为 nil）：CLI 侧传 conn.Manager 的 Get——连接跨
// 主机复用 + 自愈，统一 CloseAll 收尾，单主机读毕不关；console 侧直拨
// agentc 并在单主机读毕即关。
type ConnFactory func(ctx context.Context, h *model.Host) (c conn.Conn, release func(), err error)

// Read 并发读取各主机 release marker 并按 missing/legacy/failed 分类，
// 结果按 hosts 声明序返回（错误清单次序不随并发完成序漂移）。marker 以
// root 读取（0600 含 values 内容），脚本与两份旧实现逐字一致：cat 失败
// 回显哨兵而非让脚本退出码非零——缺失与不可达必须分开归因。
func Read(ctx context.Context, markerPath string, hosts []*model.Host, dial ConnFactory, opts Options) []HostResult {
	script := fmt.Sprintf("cat -- %s 2>/dev/null || echo __MISSING__", shellquote.Quote(markerPath))
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	results := make([]HostResult, len(hosts))
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h *model.Host) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = readOne(ctx, script, h, dial, opts)
		}(i, h)
	}
	wg.Wait()
	return results
}

// readOne 读取单台主机：任何失败都折叠进 KindFailed 的对应成因字段，
// 不在此拼错误文案——CLI 与 console 的 failed 明细排版不同（如退出码
// 的 stderr 呈现），格式化权留在调用方。
func readOne(ctx context.Context, script string, h *model.Host, dial ConnFactory, opts Options) HostResult {
	cn, release, err := dial(ctx, h)
	if err != nil {
		return HostResult{Kind: KindFailed, Err: err}
	}
	if release != nil {
		defer release()
	}
	if opts.PerHostTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.PerHostTimeout)
		defer cancel()
	}
	res, err := cn.Exec(ctx, conn.ExecRequest{Script: script, BecomeUser: "root", TimeoutMs: 30_000})
	switch {
	case err != nil:
		return HostResult{Kind: KindFailed, Err: err}
	case res.Code != 0:
		return HostResult{Kind: KindFailed, Code: res.Code, Stderr: res.Stderr}
	}
	stdout := strings.TrimSpace(res.Stdout)
	if stdout == "__MISSING__" || stdout == "" {
		return HostResult{Kind: KindMissing}
	}
	mk, perr := chart.ParseMarker([]byte(stdout))
	if perr != nil {
		return HostResult{Kind: KindFailed, ParseErr: perr}
	}
	// legacy 口径取两旧实现的并集：schema < v2，或 v2 却缺 values 字段
	//（v2 的意义就是携带 resolved values，缺字段视同旧 marker 处理）。
	if mk.Schema() < chart.MarkerSchemaV2 || mk.Values == nil {
		return HostResult{Kind: KindLegacy}
	}
	return HostResult{Kind: KindOK, Marker: mk}
}

// Validate 对逐主机 values 做聚合校验（required + values schema + 子 chart
// 走查，与部署相位同强度，错误统一带 host 前缀）。相同 values 只校验一次
// （多主机典型情形），归因取相同 values 的字典序首台主机——报错主机不随
// map 迭代序漂移；JSON 序列化失败的 values（YAML .nan/.inf 等）按主机名
// 排序单独报错——此前 CLI 版把 marshal 错误吞掉，坏 values 之间互判相等
// 而跳过校验。
func Validate(ch *chart.Chart, hostValues map[string]map[string]any) error {
	names := make([]string, 0, len(hostValues))
	for name := range hostValues {
		names = append(names, name)
	}
	slices.Sort(names)
	seen := map[string]bool{}
	var unserializable []string
	for _, host := range names {
		v := hostValues[host]
		key, err := json.Marshal(v)
		if err != nil {
			unserializable = append(unserializable, fmt.Sprintf("host %s: %v", host, err))
			continue
		}
		if seen[string(key)] {
			continue
		}
		seen[string(key)] = true
		if err := validateOne(ch, v, host); err != nil {
			return err
		}
	}
	if len(unserializable) > 0 {
		return fmt.Errorf("marker values not JSON-serializable: %s", strings.Join(unserializable, "; "))
	}
	return nil
}

// validateOne 按部署相位同等强度校验一份 values：三项依次进行，首个失败
// 即返回，错误带主机名（两旧实现的逐项包装口径一致）。
func validateOne(ch *chart.Chart, values map[string]any, host string) error {
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

// FirstValues 多主机 marker values 不一致时按主机名排序取首台的 values
// （确定性：run 级代表性 values 不随 map 迭代顺序漂移；空集返回 nil）。
func FirstValues(hostValues map[string]map[string]any) map[string]any {
	if len(hostValues) == 0 {
		return nil
	}
	names := make([]string, 0, len(hostValues))
	for name := range hostValues {
		names = append(names, name)
	}
	slices.Sort(names)
	return hostValues[names[0]]
}
