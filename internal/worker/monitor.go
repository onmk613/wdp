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

	"wdp/internal/store"
)

// ---- Prometheus 文本格式解析 ----

// MetricSample 解析出的一条样本。
type MetricSample struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Value  float64           `json:"value"`
}

// key 是样本的序列键（name + 排序后的标签）。
func (m MetricSample) key() string {
	if len(m.Labels) == 0 {
		return m.Name
	}
	parts := make([]string, 0, len(m.Labels))
	for k, v := range m.Labels {
		parts = append(parts, k+"="+v)
	}
	// 挂载点/设备名拼接顺序稳定即可（同一次输出内一致）
	return m.Name + "{" + strings.Join(parts, ",") + "}"
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
			m.Labels[kv[1]] = strings.ReplaceAll(kv[2], `\"`, `"`)
		}
		out = append(out, m)
	}
	return out
}

// sampleIndex 建索引用于快速取值/求和。
type sampleIndex struct {
	byName map[string][]MetricSample // name → 同名全部序列
	byKey  map[string]float64
}

func indexSamples(ss []MetricSample) *sampleIndex {
	idx := &sampleIndex{byName: map[string][]MetricSample{}, byKey: map[string]float64{}}
	for _, s := range ss {
		idx.byName[s.Name] = append(idx.byName[s.Name], s)
		idx.byKey[s.key()] = s.Value
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
	diskIO              float64
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
			continue
		}
		wg.Add(1)
		go func(id int64, h *store.Host) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			body, err := m.Fetch(ctx, h)
			if err != nil {
				// 离线：置一条 crit 告警（探活状态由 Prober 维护）
				_ = m.Store.SetHostAlert(id, "offline", "crit", "agent 不可达："+firstLineOf(err.Error()), 0)
				return
			}
			_ = m.Store.ClearHostAlert(id, "offline")
			m.IngestSamples(id, ParsePromText(body))
		}(id, h)
	}
	wg.Wait()
}

// IngestSamples 派生瞬时指标并写桶、评估告警。
func (m *Monitor) IngestSamples(hostID int64, ss []MetricSample) {
	idx := indexSamples(ss)
	now := time.Now()
	bucket := now.Unix() - now.Unix()%300

	pts := []store.MetricPoint{}
	fsList := []fsUsage{} // 全部挂载点使用率（阈值评估顺序无关）
	store1 := func(metric, labels string, v float64) {
		pts = append(pts, store.MetricPoint{Metric: metric, Labels: labels, V: v})
	}

	// 比率类（无需差分）
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
	for _, fs := range idx.byName["node_filesystem_size_bytes"] {
		mount := fs.Labels["mount"]
		if mount == "" {
			continue
		}
		if a, ok := idx.labeled("node_filesystem_avail_bytes", "mount", mount); ok && fs.Value > 0 {
			pct := 100 * (fs.Value - a.Value) / fs.Value
			store1("fs_used_pct", "mount="+mount, pct)
			// 全部挂载点收集完再统一评估（顺序无关）——逐点即时评估会把
			// 同轮靠后的正常挂载点误判成"恢复"
			fsList = append(fsList, fsUsage{mount: mount, pct: pct})
		}
	}
	m.alarmFS(hostID, fsList)
	for _, l := range []struct{ m string }{{"node_load1"}, {"node_load5"}} {
		if v := idx.sum(l.m); v > 0 {
			store1(strings.TrimPrefix(l.m, "node_"), "", v)
		}
	}

	// 速率/差分类（需要上次计数器）
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
	cur.diskIO = idx.sum("node_disk_io_time_seconds_total")

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
		if v, ok := rate(cur.diskIO, prev.diskIO); ok {
			store1("disk_busy_pct", "", 100*v)
		}
	}
	m.states.Store(hostID, cur)
	if len(pts) > 0 {
		if err := m.Store.BatchUpsertMetric5m(hostID, bucket, pts); err != nil {
			m.Logger.Warn("metrics batch upsert failed", "host", hostID, "err", err)
		}
	}
}

func prevTime(has bool, p *scrapeState, now time.Time) time.Time {
	if has {
		return p.ts
	}
	return now
}

// alarm 评估阈值并写/清告警（crit ≥95%、warn ≥85%）。
func (m *Monitor) alarm(hostID int64, kind string, v float64, what, human string) {
	set := func(level string) {
		if err := m.Store.SetHostAlert(hostID, kind, level, fmt.Sprintf("%s %s（阈值 %s）", what, human, map[string]string{"crit": "≥95%", "warn": "≥85%"}[level]), v); err != nil {
			m.Logger.Warn("alert set failed", "err", err)
		}
	}
	switch {
	case v >= 95:
		set("crit")
	case v >= 85:
		set("warn")
	default:
		_ = m.Store.ClearHostAlert(hostID, kind)
	}
}

// alarmFS 文件系统阈值（crit ≥95%、warn ≥88%）：任一挂载点超阈值即告警
// （detail 取最严重挂载点——alert 粒度按主机），全部正常才清。
func (m *Monitor) alarmFS(hostID int64, mounts []fsUsage) {
	var worst fsUsage
	for _, mnt := range mounts {
		if mnt.pct > worst.pct {
			worst = mnt
		}
	}
	set := func(level string) {
		if err := m.Store.SetHostAlert(hostID, "fs", level, fmt.Sprintf("挂载点 %s 使用率 %.1f%%（阈值 %s）", worst.mount, worst.pct, map[string]string{"crit": "≥95%", "warn": "≥88%"}[level]), worst.pct); err != nil {
			m.Logger.Warn("alert set failed", "err", err)
		}
	}
	switch {
	case worst.pct >= 95:
		set("crit")
	case worst.pct >= 88:
		set("warn")
	default:
		_ = m.Store.ClearHostAlert(hostID, "fs")
	}
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}
