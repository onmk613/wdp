package worker

// 主机监控采样：agent /metrics 的解析与周期任务。
//   - 采样：Monitor 每分钟抓一次 → 派生使用率/速率（计数器差分）→
//     并入 5 分钟聚合桶（保留 30 天）→ 阈值评估写 host_alerts（页面
//     红黄标记；真正的告警链路是 Prometheus 抓 web 的代理端点）。

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"wdp/internal/fmtutil"
	"wdp/internal/store"
)

// ---- Prometheus 文本格式解析 ----

// MetricSample 解析出的一条样本。
type MetricSample struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Value  float64           `json:"value"`
}

var labelRe = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

// ParsePromText 解析 Prometheus 文本格式（忽略 # 注释行与时间戳列）。
func ParsePromText(body string) []MetricSample {
	out := []MetricSample{}
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 256<<10), 256<<10)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		iBrace := strings.IndexByte(line, '{')
		iSp := strings.IndexByte(line, ' ')
		var name, labelsRaw, rest string
		switch {
		case iBrace > 0 && (iSp < 0 || iBrace < iSp):
			j := strings.IndexByte(line, '}')
			if j < 0 || j < iBrace {
				continue
			}
			name, labelsRaw, rest = line[:iBrace], line[iBrace+1:j], line[j+1:]
		case iSp > 0:
			name, rest = line[:iSp], line[iSp:]
		default:
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		m := MetricSample{Name: name, Value: v, Labels: map[string]string{}}
		for _, kv := range labelRe.FindAllStringSubmatch(labelsRaw, -1) {
			m.Labels[kv[1]] = unescapeLabelValue(kv[2])
		}
		out = append(out, m)
	}
	return out
}

// unescapeLabelValue 还原 PromText label 值的转义序列（\\、\"、\n——
// 与 Prometheus 文本暴露格式的转义集合一致）。此前只还原 \"，含反斜杠
// 或换行转义的值（如挂载点 "\\srv\share"、多行 label）会带着字面转义
// 入库，检索与告警匹配都对不上原始值。单趟顺序扫描而非链式 ReplaceAll：
// `\\n` 是"反斜杠 + n"，链式先把 `\\` 换成 `\` 再把 `\n` 当换行会过度
// 还原；未知转义序列原样保留（宽容解析，与库行为一致）。
func unescapeLabelValue(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '\\':
			b.WriteByte('\\')
		case '"':
			b.WriteByte('"')
		case 'n':
			b.WriteByte('\n')
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i+1])
		}
		i++
	}
	return b.String()
}

// sampleIndex 建索引用于快速取值/求和。
type sampleIndex struct {
	byName map[string][]MetricSample // name → 同名全部序列
}

func indexSamples(ss []MetricSample) *sampleIndex {
	idx := &sampleIndex{byName: map[string][]MetricSample{}}
	for _, s := range ss {
		idx.byName[s.Name] = append(idx.byName[s.Name], s)
	}
	return idx
}

// sum 对同名单标签求和（计数器聚合）。
func (idx *sampleIndex) sum(name string) float64 {
	t := 0.0
	for _, s := range idx.byName[name] {
		t += s.Value
	}
	return t
}

// labeled 取某标签值匹配的首个样本。
func (idx *sampleIndex) labeled(name, label, want string) (MetricSample, bool) {
	for _, s := range idx.byName[name] {
		if s.Labels[label] == want {
			return s, true
		}
	}
	return MetricSample{}, false
}

// ---- 周期采样 ----

// scrapeState 是逐主机的计数器快照（速率类派生需要差分）。
type scrapeState struct {
	ts                  time.Time
	cpuTotal, cpuIdle   float64
	netRx, netTx        float64
	diskRead, diskWrite float64
	diskIO              map[string]float64 // device → 累计 io_time 秒（逐盘差分）
}

// fsUsage 单挂载点使用率（告警评估用）。
type fsUsage struct {
	mount string
	pct   float64
}

// Monitor 周期采样器。Fetch 由传输层注入（mTLS/明文 scheme 选择在 web）。
type Monitor struct {
	Store  *store.Store
	Logger *slog.Logger
	Fetch  func(ctx context.Context, h *store.Host) (string, error)

	states sync.Map // hostID → *scrapeState
}

// Forget 主机删除时清理差分快照（防下一台同 ID 主机拿到旧计数器算出
// 负速率）。
func (m *Monitor) Forget(hostID int64) { m.states.Delete(hostID) }

// Run 阻塞执行采样循环：每分钟采样一轮，每小时清理 30 天前的桶。
func (m *Monitor) Run(ctx context.Context) {
	m.scrapeOnce(ctx) // 启动即先采一轮
	t := time.NewTicker(time.Minute)
	prune := time.NewTicker(time.Hour)
	defer t.Stop()
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.scrapeOnce(ctx)
		case <-prune.C:
			if err := m.Store.PruneMetrics(time.Now().Add(-30 * 24 * time.Hour).Unix()); err != nil {
				m.Logger.Warn("metrics prune failed", "err", err)
			}
		}
	}
}

func (m *Monitor) scrapeOnce(ctx context.Context) {
	ids, err := m.Store.HostIDs()
	if err != nil {
		m.Logger.Error("scrape: list hosts", "err", err)
		return
	}
	// 有界并发抓取：串行时一台超时就拖累整轮（主机多了 1 分钟周期装不
	// 下）；8 并发 + 单机超时控制，整轮时延 ≈ 主机数/8 × 最慢单机
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		h, err := m.Store.GetHost(id)
		if err != nil {
			m.Logger.Warn("scrape: get host", "id", id, "err", err)
			continue
		}
		wg.Add(1)
		go func(id int64, h *store.Host) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			body, err := m.Fetch(ctx, h)
			// ctx 已取消（server 关停）：已派发 goroutine 的抓取以失败收场，
			// 据此写 offline crit 告警会把全网打成不可达——关停不应污染告警
			// 面板，下一轮采样自会给出真实结论
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				// 离线：置一条 crit 告警（探活状态由 Prober 维护）
				_ = m.Store.SetHostAlert(id, "offline", "crit", "agent 不可达："+fmtutil.FirstLine(err.Error()), 0)
				return
			}
			_ = m.Store.ClearHostAlert(id, "offline")
			m.IngestSamples(id, ParsePromText(body))
		}(id, h)
	}
	wg.Wait()
}

// IngestSamples 派生瞬时指标并写桶、评估告警：按"比率类 / 文件系统类 /
// 计数器差分类"三类派生（见各自方法），最后单事务并入 5 分钟桶。
func (m *Monitor) IngestSamples(hostID int64, ss []MetricSample) {
	idx := indexSamples(ss)
	now := time.Now()
	bucket := now.Unix() - now.Unix()%300

	pts := []store.MetricPoint{}
	store1 := func(metric, labels string, v float64) {
		pts = append(pts, store.MetricPoint{Metric: metric, Labels: labels, V: v})
	}

	m.ingestRatios(hostID, idx, store1)
	m.ingestFilesystems(hostID, idx, store1)
	m.ingestCounters(hostID, idx, now, store1)

	if len(pts) > 0 {
		if err := m.Store.BatchUpsertMetric5m(hostID, bucket, pts); err != nil {
			m.Logger.Warn("metrics batch upsert failed", "host", hostID, "err", err)
		}
	}
}

// ingestRatios 比率类（无需差分）：内存 / swap / 负载。
func (m *Monitor) ingestRatios(hostID int64, idx *sampleIndex, store1 func(metric, labels string, v float64)) {
	if t := idx.sum("node_memory_MemTotal_bytes"); t > 0 {
		if a := idx.sum("node_memory_MemAvailable_bytes"); a > 0 {
			store1("mem_used_pct", "", 100*(t-a)/t)
			m.alarm(hostID, "mem", 100*(t-a)/t, "内存使用率", fmt.Sprintf("%.1f%%", 100*(t-a)/t))
		}
	}
	if st := idx.sum("node_memory_SwapTotal_bytes"); st > 0 {
		sf := idx.sum("node_memory_SwapFree_bytes")
		pct := 100 * (st - sf) / st
		store1("swap_used_pct", "", pct)
	}
	for _, l := range []struct{ m string }{{"node_load1"}, {"node_load5"}} {
		if v := idx.sum(l.m); v > 0 {
			store1(strings.TrimPrefix(l.m, "node_"), "", v)
		}
	}
}

// ingestFilesystems 文件系统类：逐挂载点使用率入库并评估告警。全部
// 挂载点收集完再统一评估（顺序无关）——逐点即时评估会把同轮靠后的
// 正常挂载点误判成"恢复"。
func (m *Monitor) ingestFilesystems(hostID int64, idx *sampleIndex, store1 func(metric, labels string, v float64)) {
	fsList := []fsUsage{} // 全部挂载点使用率（阈值评估顺序无关）
	for _, fs := range idx.byName["node_filesystem_size_bytes"] {
		mount := fs.Labels["mount"]
		if mount == "" {
			continue
		}
		if a, ok := idx.labeled("node_filesystem_avail_bytes", "mount", mount); ok && fs.Value > 0 {
			pct := 100 * (fs.Value - a.Value) / fs.Value
			store1("fs_used_pct", "mount="+mount, pct)
			fsList = append(fsList, fsUsage{mount: mount, pct: pct})
		}
	}
	m.alarmFS(hostID, fsList)
}

// ingestCounters 计数器差分类：CPU / 网络 / 磁盘速率与忙碌度（需要上次
// 计数器快照差分），差分完落本轮快照供下轮使用。
func (m *Monitor) ingestCounters(hostID int64, idx *sampleIndex, now time.Time, store1 func(metric, labels string, v float64)) {
	prevAny := false
	stAny, _ := m.states.Load(hostID)
	var prev *scrapeState
	if stAny != nil {
		if p, ok := stAny.(*scrapeState); ok && p.ts.Before(now) {
			prev, prevAny = p, true
		}
	}
	cur := &scrapeState{ts: now}
	for _, smp := range idx.byName["node_cpu_seconds_total"] {
		switch smp.Labels["mode"] {
		case "idle", "iowait":
			cur.cpuIdle += smp.Value
		}
		cur.cpuTotal += smp.Value
	}
	cur.netRx = idx.sum("node_network_receive_bytes_total")
	cur.netTx = idx.sum("node_network_transmit_bytes_total")
	cur.diskRead = idx.sum("node_disk_read_bytes_total")
	cur.diskWrite = idx.sum("node_disk_written_bytes_total")
	// io_time 按设备快照：跨设备求和再差分会让 N 块盘的忙碌度叠成 N×100%
	// （busy% 是单设备定义）——逐盘分别记，与 fs 指标逐 mount 入库同款
	cur.diskIO = map[string]float64{}
	for _, smp := range idx.byName["node_disk_io_time_seconds_total"] {
		if dev := smp.Labels["device"]; dev != "" {
			cur.diskIO[dev] = smp.Value
		}
	}

	// 计数器回绕/清零（主机重启）时差分非正，速率照算会入库负值——
	// 该指标本轮跳过，等下一轮重新建立基线；CPU 百分比按定义域钳到 [0,100]
	if prevAny && cur.cpuTotal > prev.cpuTotal {
		cpuPct := 100 * (1 - (cur.cpuIdle-prev.cpuIdle)/(cur.cpuTotal-prev.cpuTotal))
		if cpuPct < 0 {
			cpuPct = 0
		}
		if cpuPct > 100 {
			cpuPct = 100
		}
		store1("cpu_usage_pct", "", cpuPct)
		m.alarm(hostID, "cpu", cpuPct, "CPU 使用率", fmt.Sprintf("%.1f%%", cpuPct))
	}
	dt := now.Sub(prevTime(prevAny, prev, now)).Seconds()
	if prevAny && dt > 0 {
		rate := func(curV, prevV float64) (float64, bool) {
			d := curV - prevV
			return d / dt, d > 0
		}
		if v, ok := rate(cur.netRx, prev.netRx); ok {
			store1("net_rx_bps", "", v)
		}
		if v, ok := rate(cur.netTx, prev.netTx); ok {
			store1("net_tx_bps", "", v)
		}
		if v, ok := rate(cur.diskRead, prev.diskRead); ok {
			store1("disk_read_bps", "", v)
		}
		if v, ok := rate(cur.diskWrite, prev.diskWrite); ok {
			store1("disk_write_bps", "", v)
		}
		for dev, nowIO := range cur.diskIO {
			prevIO, has := prev.diskIO[dev]
			if !has {
				continue // 新盘无差分基线：本轮跳过，下一轮重建（与回绕同口径）
			}
			if v, ok := rate(nowIO, prevIO); ok {
				store1("disk_busy_pct", "device="+dev, 100*v)
			}
		}
	}
	m.states.Store(hostID, cur)
}

func prevTime(has bool, p *scrapeState, now time.Time) time.Time {
	if has {
		return p.ts
	}
	return now
}

// 阈值（百分比）：crit 通用 95；warn 对 CPU/内存为 85，文件系统放宽到
// 88。switch 判定与告警文案共用这几个常量，避免两处口径漂移。
const (
	critPct   = 95
	warnPct   = 85
	warnFsPct = 88
)

// pctLabel 生成告警文案里的阈值表示（如 "≥95%"）。
func pctLabel(p int) string { return fmt.Sprintf("≥%d%%", p) }

// alarm 评估阈值并写/清告警（crit ≥critPct、warn ≥warnPct）。
func (m *Monitor) alarm(hostID int64, kind string, v float64, what, human string) {
	set := func(level string) {
		if err := m.Store.SetHostAlert(hostID, kind, level, fmt.Sprintf("%s %s（阈值 %s）", what, human, map[string]string{"crit": pctLabel(critPct), "warn": pctLabel(warnPct)}[level]), v); err != nil {
			m.Logger.Warn("alert set failed", "err", err)
		}
	}
	switch {
	case v >= critPct:
		set("crit")
	case v >= warnPct:
		set("warn")
	default:
		_ = m.Store.ClearHostAlert(hostID, kind)
	}
}

// alarmFS 文件系统阈值（crit ≥critPct、warn ≥warnFsPct）：任一挂载点超
// 阈值即告警（detail 取最严重挂载点——alert 粒度按主机），全部正常才清。
func (m *Monitor) alarmFS(hostID int64, mounts []fsUsage) {
	var worst fsUsage
	for _, mnt := range mounts {
		if mnt.pct > worst.pct {
			worst = mnt
		}
	}
	set := func(level string) {
		if err := m.Store.SetHostAlert(hostID, "fs", level, fmt.Sprintf("挂载点 %s 使用率 %.1f%%（阈值 %s）", worst.mount, worst.pct, map[string]string{"crit": pctLabel(critPct), "warn": pctLabel(warnFsPct)}[level]), worst.pct); err != nil {
			m.Logger.Warn("alert set failed", "err", err)
		}
	}
	switch {
	case worst.pct >= critPct:
		set("crit")
	case worst.pct >= warnFsPct:
		set("warn")
	default:
		_ = m.Store.ClearHostAlert(hostID, "fs")
	}
}
