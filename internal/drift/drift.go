// Package drift 实现跨主机漂移巡检的检测内核：读取各主机的 release
// marker（<marker_dir>/<chart>/release.json），与当前 chart 合并 values 的
// 摘要（chart.ValuesDigest，与 marker 写入口径一致）比对分类。命令行
// 装配（inventory/executor/连接）在 internal/cli，本包只做检测与分类，
// 只读不收敛——纠偏动作明确留给 `wdp run`（幂等重部署）。
//
// 逐主机结论：
//
//	OK           marker 摘要 = 当前 values 摘要
//	DRIFTED      values 已变（改配置未部署，或有人手改过现场）
//	OUTDATED     部署的是旧 chart 版本（values 摘要一致）
//	NOT-DEPLOYED 无 marker（未部署过或已卸载）
//	UNREACHABLE  连接失败
//
// 判失败口径：DRIFTED / UNREACHABLE（可直接进 CI）；NOT-DEPLOYED /
// OUTDATED 只列出不判失败（扩容中主机与待升级是常态，不是漂移）。
package drift

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"wdp/internal/chart"
	"wdp/internal/model"
	"wdp/internal/report"
	"wdp/internal/shellquote"
)

// Capture 包裹真实 reporter：静默收集单任务逐主机结果（stdout/失败态），
// 其余事件透传给 inner。巡检 play 执行完由 Classify 消费收集结果。
type Capture struct {
	inner report.Reporter
	mu    sync.Mutex
	out   map[string]*model.TaskResult
}

// NewCapture 构造收集器。
func NewCapture(inner report.Reporter) *Capture {
	return &Capture{inner: inner, out: map[string]*model.TaskResult{}}
}

func (c *Capture) PlayStart(name string, hosts []string) { c.inner.PlayStart(name, hosts) }
func (c *Capture) TaskStart(task, module string)         { c.inner.TaskStart(task, module) }
func (c *Capture) TaskDone()                             { c.inner.TaskDone() }
func (c *Capture) PlayMsg(format string, a ...any)       { c.inner.PlayMsg(format, a...) }
func (c *Capture) Recap(n string, s map[string]*model.Stats) {
	c.inner.Recap(n, s)
}
func (c *Capture) Finish() { c.inner.Finish() }

func (c *Capture) HostResult(host string, r *model.TaskResult) {
	c.mu.Lock()
	c.out[host] = new(*r)
	c.mu.Unlock()
	c.inner.HostResult(host, r)
}

// Results 返回收集到的逐主机结果（巡检 play 执行完后调用）。
func (c *Capture) Results() map[string]*model.TaskResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out
}

// Row 一行巡检结论。
type Row struct {
	Host, State, Detail string
}

// ReadMarkerPlay 构造读取全部主机 marker 的只读 shell play（marker 0600
// 含 values 内容，读取需 become root；缺失时输出 __MISSING__ 哨兵）。
func ReadMarkerPlay(chartName, markerPath, pattern string) *model.Play {
	return &model.Play{
		Name:   fmt.Sprintf("drift check: %s", chartName),
		Hosts:  pattern,
		Become: true,
		Tasks: []*model.Task{{
			Name:        "read release marker",
			Module:      "shell",
			FreeForm:    fmt.Sprintf("cat -- %s 2>/dev/null || echo __MISSING__", shellquote.Quote(markerPath)),
			ChangedWhen: "{{ false }}",
		}},
	}
}

// Classify 逐主机比对 marker 读取结果与期望摘要，产出结论行，并返回需判
// 失败的主机数（DRIFTED / UNREACHABLE）。wantValues 非空且 marker 为 v2
// （含 resolved values）时，DRIFTED 的 Detail 升级为字段级 diff。
func Classify(hosts []*model.Host, results map[string]*model.TaskResult, chartVersion, wantSHA string, wantValues map[string]any) (rows []Row, failed int) {
	for _, h := range hosts {
		r := results[h.Name]
		if r == nil || r.Unreachable {
			rows = append(rows, Row{h.Name, "UNREACHABLE", orDash(r)})
			failed++
			continue
		}
		if r.Failed {
			rows = append(rows, Row{h.Name, "UNREACHABLE", firstLineOr(r.Msg, "read failed")})
			failed++
			continue
		}
		stdout := strings.TrimSpace(r.Stdout)
		if stdout == "__MISSING__" || stdout == "" {
			rows = append(rows, Row{h.Name, "NOT-DEPLOYED", "no release marker"})
			continue
		}
		mk, err := chart.ParseMarker([]byte(stdout))
		if err != nil {
			rows = append(rows, Row{h.Name, "DRIFTED", "marker unreadable: " + firstLineOr(err.Error(), "bad json")})
			failed++
			continue
		}
		switch {
		case mk.ValuesSHA != wantSHA:
			rows = append(rows, Row{h.Name, "DRIFTED", driftedDetail(mk, wantSHA, wantValues)})
			failed++
		case mk.Version != chartVersion:
			rows = append(rows, Row{h.Name, "OUTDATED",
				fmt.Sprintf("deployed %s, chart is %s", orNA(mk.Version), chartVersion)})
		default:
			rows = append(rows, Row{h.Name, "OK", fmt.Sprintf("%s (deployed %s)", orNA(mk.Version), orNA(mk.DeployedAt))})
		}
	}
	return rows, failed
}

// driftedDetail 生成 DRIFTED 行的说明：marker v2 且给了当前 values 时输出
// 字段级差异（脱敏占位不构成差异），否则维持摘要口径。
func driftedDetail(mk *chart.Marker, wantSHA string, wantValues map[string]any) string {
	summary := fmt.Sprintf("deployed values sha %s != current %s (deployed %s)",
		orNA(mk.ValuesSHA), wantSHA, orNA(mk.DeployedAt))
	if mk.Schema() < chart.MarkerSchemaV2 || len(mk.Values) == 0 || wantValues == nil {
		return summary
	}
	if diffs := ValuesDiff(mk.Values, wantValues); len(diffs) > 0 {
		slices.Sort(diffs)
		if len(diffs) > 5 {
			diffs = append(diffs[:5], fmt.Sprintf("… and %d more", len(diffs)-5))
		}
		return summary + "; fields: " + strings.Join(diffs, ", ")
	}
	return summary
}

// ValuesDiff 递归比较两棵 values 树，返回叶子级差异（"port: 9100 → 9200"）。
// "<redacted>" 占位（sensitive_values 脱敏键）不构成差异——真实敏感值的
// 变化不参与摘要，也不应在 diff 里泄露。
func ValuesDiff(deployed, current map[string]any) []string {
	var out []string
	var walk func(a, b map[string]any, prefix string)
	walk = func(a, b map[string]any, prefix string) {
		for k, av := range a {
			bv, ok := b[k]
			if s, isStr := av.(string); isStr && s == chart.RedactedValue {
				continue // 脱敏占位，无法比较
			}
			if !ok {
				out = append(out, fmt.Sprintf("%s%s: %v → (removed)", prefix, k, av))
				continue
			}
			am, aIsMap := av.(map[string]any)
			bm, bIsMap := bv.(map[string]any)
			switch {
			case aIsMap && bIsMap:
				walk(am, bm, prefix+k+".")
			case av != bv:
				out = append(out, fmt.Sprintf("%s%s: %v → %v", prefix, k, av, bv))
			}
		}
		for k, bv := range b {
			if _, ok := a[k]; !ok {
				out = append(out, fmt.Sprintf("%s%s: (absent) → %v", prefix, k, bv))
			}
		}
	}
	walk(deployed, current, "")
	return out
}

func orDash(r *model.TaskResult) string {
	if r == nil || r.Msg == "" {
		return "unreachable"
	}
	return firstLineOr(r.Msg, "unreachable")
}

func orNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}

func firstLineOr(s, def string) string {
	if s == "" {
		return def
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
