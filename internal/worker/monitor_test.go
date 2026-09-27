package worker

// 采样链路：Prometheus 文本解析 → 派生指标入桶（5m 聚合 upsert）→
// 阈值告警评估（写/清）→ 计数器回绕防御。

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/store"
)

const sampleMetrics = `# HELP node_cpu_seconds_total cpu
# TYPE node_cpu_seconds_total counter
node_cpu_seconds_total{cpu="0",mode="idle"} 90
node_cpu_seconds_total{cpu="0",mode="user"} 10
node_cpu_seconds_total{cpu="1",mode="idle"} 80
node_cpu_seconds_total{cpu="1",mode="user"} 20
node_load1 0.5
node_memory_MemTotal_bytes 8000000000
node_memory_MemAvailable_bytes 2000000000
node_filesystem_size_bytes{mount="/",device="/dev/sda1"} 10000000000
node_filesystem_avail_bytes{mount="/",device="/dev/sda1"} 1000000000
node_network_receive_bytes_total{device="eth0"} 1000
node_disk_io_time_seconds_total{device="sda"} 5
`

// newMonitor 测试装配：内存临时库 + 直采 Monitor。
func newMonitor(t *testing.T) (*Monitor, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "wdp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := &Monitor{Store: st, Logger: slog.New(slog.DiscardHandler)}
	return m, st
}

func TestParsePromText(t *testing.T) {
	ss := ParsePromText(sampleMetrics)
	idx := indexSamples(ss)
	if got := idx.sum("node_cpu_seconds_total"); got != 200 {
		t.Fatalf("cpu 总和应 200: %v", got)
	}
	if got := idx.sum("node_memory_MemTotal_bytes"); got != 8e9 {
		t.Fatalf("memtotal: %v", got)
	}
	if a, ok := idx.labeled("node_filesystem_avail_bytes", "mount", "/"); !ok || a.Value != 1e9 {
		t.Fatalf("fs avail: %+v", a)
	}
	// JSON 端点形态：labels 解析为 map
	for _, s := range ss {
		if s.Name == "node_cpu_seconds_total" && s.Labels["cpu"] == "0" && s.Labels["mode"] == "idle" && s.Value == 90 {
			return
		}
	}
	t.Fatalf("标签解析异常: %+v", ss[:4])
}

func TestIngestSamplesAggAndAlerts(t *testing.T) {
	m, st := newMonitor(t)
	id, err := st.CreateHost(&store.Host{Name: "mon-host", Address: "127.0.0.1", AgentPort: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 两轮采样：内存 75% 无告警；fs 90% → warn
	m.IngestSamples(id, ParsePromText(sampleMetrics))
	// 第二轮（差分生效）
	ss := ParsePromText(sampleMetrics)
	for i := range ss {
		if ss[i].Name == "node_cpu_seconds_total" || ss[i].Name == "node_network_receive_bytes_total" {
			ss[i].Value += 30
		}
	}
	m.IngestSamples(id, ss)

	alerts, err := st.ListHostAlerts()
	if err != nil {
		t.Fatal(err)
	}
	var hasFSWarn, hasMem bool
	for _, a := range alerts {
		if a.Kind == "fs" && a.Level == "warn" {
			hasFSWarn = true
		}
		if a.Kind == "mem" {
			hasMem = true
		}
	}
	if !hasFSWarn {
		t.Fatalf("fs 90%% 应 warn: %+v", alerts)
	}
	if hasMem {
		t.Fatalf("内存 75%% 不应告警: %+v", alerts)
	}
	// 桶聚合：cpu_usage_pct（两轮 idle 差 0 → 100%）与 mem_used_pct 各应有样本
	for _, metric := range []string{"cpu_usage_pct", "mem_used_pct", "net_rx_bps"} {
		pts, err := st.QuerySeries(id, metric, "", 0)
		if err != nil || len(pts) == 0 {
			t.Fatalf("系列 %s 应有样本: %v %v", metric, pts, err)
		}
	}
	// mem_used_pct = 75
	pts, _ := st.QuerySeries(id, "mem_used_pct", "", 0)
	if pts[0].Avg != 75 {
		t.Fatalf("mem_used_pct 应 75: %v", pts[0])
	}
	// 恢复：内存可用放大 → 告警清空（fs 恢复正常挂载）
	ok := `node_memory_MemTotal_bytes 8000000000
node_memory_MemAvailable_bytes 7900000000
node_filesystem_size_bytes{mount="/"} 10000000000
node_filesystem_avail_bytes{mount="/"} 9900000000
`
	m.IngestSamples(id, ParsePromText(ok))
	alerts, _ = st.ListHostAlerts()
	if len(alerts) != 0 {
		t.Fatalf("恢复后应无告警: %+v", alerts)
	}
}

// TestAlarmFSAnyMountOver 任一挂载点超阈值即告警（顺序无关）：同轮里
// 靠后的正常挂载点不得清掉告警；全部恢复才清。
func TestAlarmFSAnyMountOver(t *testing.T) {
	m, st := newMonitor(t)
	id, err := st.CreateHost(&store.Host{Name: "fs-host", Address: "127.0.0.1", AgentPort: 1})
	if err != nil {
		t.Fatal(err)
	}
	over := `node_filesystem_size_bytes{mount="/"} 10000000000
node_filesystem_avail_bytes{mount="/"} 100000000
node_filesystem_size_bytes{mount="/data"} 10000000000
node_filesystem_avail_bytes{mount="/data"} 9000000000
`
	m.IngestSamples(id, ParsePromText(over))
	alerts, _ := st.ListHostAlerts()
	var fs *store.HostAlert
	for _, a := range alerts {
		if a.Kind == "fs" {
			fs = a
		}
	}
	if fs == nil || fs.Level != "crit" || !strings.Contains(fs.Detail, "/ ") || strings.Contains(fs.Detail, "/data") {
		t.Fatalf("任一挂载点 99%% 应 crit 且报最严重挂载点: %+v", fs)
	}
	// 全部恢复才清
	ok := `node_filesystem_size_bytes{mount="/"} 10000000000
node_filesystem_avail_bytes{mount="/"} 9900000000
node_filesystem_size_bytes{mount="/data"} 10000000000
node_filesystem_avail_bytes{mount="/data"} 9900000000
`
	m.IngestSamples(id, ParsePromText(ok))
	alerts, _ = st.ListHostAlerts()
	for _, a := range alerts {
		if a.Kind == "fs" {
			t.Fatalf("全部挂载点恢复应清告警: %+v", alerts)
		}
	}
}

// TestCounterWraparound 计数器回绕/清零（主机重启）差分非正：速率不入库；
// CPU 百分比钳在 [0,100]。
func TestCounterWraparound(t *testing.T) {
	m, st := newMonitor(t)
	id, err := st.CreateHost(&store.Host{Name: "wrap-host", Address: "127.0.0.1", AgentPort: 1})
	if err != nil {
		t.Fatal(err)
	}
	m.IngestSamples(id, ParsePromText(sampleMetrics)) // netRx=1000、idle=170、total=200
	// 第二轮：netRx 回绕变小；非 idle 计数回落使 idle 差(200) > total 差(100)
	ss := ParsePromText(sampleMetrics)
	for i := range ss {
		switch {
		case ss[i].Name == "node_network_receive_bytes_total":
			ss[i].Value = 500 // 回绕
		case ss[i].Name == "node_cpu_seconds_total" && ss[i].Labels["mode"] == "idle":
			ss[i].Value += 100 // idle 差 200
		case ss[i].Name == "node_cpu_seconds_total":
			ss[i].Value -= 50 // user 差 -100 → total 差 100
		}
	}
	m.IngestSamples(id, ss)
	pts, _ := st.QuerySeries(id, "net_rx_bps", "", 0)
	if len(pts) != 0 {
		t.Fatalf("回绕差分为负不应入库: %v", pts)
	}
	pts, _ = st.QuerySeries(id, "cpu_usage_pct", "", 0)
	if len(pts) == 0 || pts[0].Avg != 0 {
		t.Fatalf("CPU 回绕形态（idle 差>total 差）应钳制为 0: %+v", pts)
	}
}

// TestDiskBusyPerDevice disk_busy_pct 逐设备入库（device 标签）：
// io_time 跨设备求和会让 N 块盘的忙碌度叠成 N×100%，破坏 busy% 的
// 单设备定义——与 fs 指标逐 mount 同款，按 device 分别成系列。
func TestDiskBusyPerDevice(t *testing.T) {
	m, st := newMonitor(t)
	id, err := st.CreateHost(&store.Host{Name: "disk-host", Address: "127.0.0.1", AgentPort: 1})
	if err != nil {
		t.Fatal(err)
	}
	base := `node_disk_io_time_seconds_total{device="sda"} 100
node_disk_io_time_seconds_total{device="sdb"} 200
`
	m.IngestSamples(id, ParsePromText(base)) // 建基线
	ss := ParsePromText(base)
	for i := range ss {
		ss[i].Value += 10 * float64(i+1) // sda +10s、sdb +20s：忙碌度比例 1:2
	}
	m.IngestSamples(id, ss)

	// 各自成系列；不再有主机级求和系列（labels=""）
	for _, dev := range []string{"sda", "sdb"} {
		pts, err := st.QuerySeries(id, "disk_busy_pct", "device="+dev, 0)
		if err != nil || len(pts) != 1 {
			t.Fatalf("device=%s 应有独立系列: %v %v", dev, pts, err)
		}
	}
	if pts, _ := st.QuerySeries(id, "disk_busy_pct", "", 0); len(pts) != 0 {
		t.Fatalf("不应写主机级求和系列: %v", pts)
	}
	// 两设备同一轮差分共用同一 dt：忙碌度比应等于 io_time 增量比 1:2
	a, _ := st.QuerySeries(id, "disk_busy_pct", "device=sda", 0)
	b, _ := st.QuerySeries(id, "disk_busy_pct", "device=sdb", 0)
	if r := b[0].Avg / a[0].Avg; r < 1.99 || r > 2.01 {
		t.Fatalf("sdb/sda 忙碌度比应 ≈2: %v vs %v", b[0].Avg, a[0].Avg)
	}
}

// TestParsePromTextLabelUnescape label 值转义还原：\\、\"、\n 三个
// PromText 转义都要还原，`\\n` 不得被过度还原成换行（先 \\ 后 n 的
// 顺序语义），未知转义原样保留。
func TestParsePromTextLabelUnescape(t *testing.T) {
	body := `node_filesystem_size_bytes{mount="\\srv\\share"} 100
node_filesystem_size_bytes{mount="line1\nline2"} 200
node_filesystem_size_bytes{mount="quo\"te"} 300
node_filesystem_size_bytes{mount="back\\nslash"} 400
node_filesystem_size_bytes{mount="unk\\twn"} 500
`
	ss := ParsePromText(body)
	got := map[string]string{}
	for _, s := range ss {
		got[s.Labels["mount"]] = "hit"
	}
	for _, want := range []string{`\srv\share`, "line1\nline2", `quo"te`, `back\nslash`, `unk\twn`} {
		if _, ok := got[want]; !ok {
			t.Fatalf("label 值 %q 未按转义语义还原: %+v", want, ss)
		}
	}
}

// TestScrapeOnceCancelledCtxNoOfflineAlert 关停瞬间伪 offline 回归：
// ctx 取消后已派发 goroutine 的抓取以失败收场，不得据此写 offline
// crit 告警——server 关停不应把全网打成不可达。对照组：ctx 存活时同一
// 抓取失败照常告警（证明失败路径可达，测试非空洞）。
func TestScrapeOnceCancelledCtxNoOfflineAlert(t *testing.T) {
	m, st := newMonitor(t)
	if _, err := st.CreateHost(&store.Host{Name: "down-host", Address: "127.0.0.1", AgentPort: 1}); err != nil {
		t.Fatal(err)
	}
	m.Fetch = func(context.Context, *store.Host) (string, error) {
		return "", errors.New("connection refused")
	}

	// 对照组：ctx 存活 → offline 告警照常写入
	m.scrapeOnce(context.Background())
	alerts, err := st.ListHostAlerts()
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) == 0 || alerts[0].Kind != "offline" || alerts[0].Level != "crit" {
		t.Fatalf("ctx 存活时抓取失败应写 offline crit: %+v", alerts)
	}

	// 清空后重放：ctx 已取消 → 不写（且不清既有告警，避免半程状态）
	if err := st.ClearHostAlert(mustFirstHostID(t, st), "offline"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.scrapeOnce(ctx)
	alerts, err = st.ListHostAlerts()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range alerts {
		if a.Kind == "offline" {
			t.Fatalf("ctx 取消后的抓取失败不应写 offline 告警: %+v", a)
		}
	}
}

// mustFirstHostID 取库中首个主机 id（测试装配辅助）。
func mustFirstHostID(t *testing.T, st *store.Store) int64 {
	t.Helper()
	ids, err := st.HostIDs()
	if err != nil || len(ids) == 0 {
		t.Fatalf("测试主机应存在: %v %v", ids, err)
	}
	return ids[0]
}

// TestForgetHostState 主机删除清差分快照：同 ID 重建主机不得拿到旧计数器。
func TestForgetHostState(t *testing.T) {
	m, st := newMonitor(t)
	id, _ := st.CreateHost(&store.Host{Name: "f", Address: "127.0.0.1", AgentPort: 1})
	m.IngestSamples(id, ParsePromText(sampleMetrics))
	if _, ok := m.states.Load(id); !ok {
		t.Fatal("采样后应有差分快照")
	}
	m.Forget(id)
	if _, ok := m.states.Load(id); ok {
		t.Fatal("Forget 后快照应清除")
	}
}
