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
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"wdp/internal/model"
	"wdp/internal/report"
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

// markerFile 与 chart.MarkerContent 的 JSON 结构保持一致（只读对比用）。
type markerFile struct {
	Chart      string `json:"chart"`
	Version    string `json:"version"`
	Phase      string `json:"phase"`
	DeployedAt string `json:"deployed_at"`
	ValuesSHA  string `json:"values_sha256"`
	WdpVersion string `json:"wdp_version"`
}

// Row 一行巡检结论。
type Row struct {
	Host, State, Detail string
}

// ReadMarkerPlay 构造读取全部主机 marker 的只读 shell play（marker 0644，
// become 不需要；缺失时输出 __MISSING__ 哨兵）。
func ReadMarkerPlay(chartName, markerPath, pattern string) *model.Play {
	return &model.Play{
		Name:  fmt.Sprintf("drift check: %s", chartName),
		Hosts: pattern,
		Tasks: []*model.Task{{
			Name:        "read release marker",
			Module:      "shell",
			FreeForm:    fmt.Sprintf("cat %s 2>/dev/null || echo __MISSING__", markerPath),
			ChangedWhen: "{{ false }}",
		}},
	}
}

// Classify 逐主机比对 marker 读取结果与期望摘要，产出结论行，并返回需判
// 失败的主机数（DRIFTED / UNREACHABLE）。
func Classify(hosts []*model.Host, results map[string]*model.TaskResult, chartVersion, wantSHA string) (rows []Row, failed int) {
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
		var mk markerFile
		if err := json.Unmarshal([]byte(stdout), &mk); err != nil {
			rows = append(rows, Row{h.Name, "DRIFTED", "marker unreadable: " + firstLineOr(err.Error(), "bad json")})
			failed++
			continue
		}
		switch {
		case mk.ValuesSHA != wantSHA:
			rows = append(rows, Row{h.Name, "DRIFTED",
				fmt.Sprintf("deployed values sha %s != current %s (deployed %s)",
					orNA(mk.ValuesSHA), wantSHA, orNA(mk.DeployedAt))})
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
