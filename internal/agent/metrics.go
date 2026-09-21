package agent

// 主机指标采集：对齐 node_exporter 的指标命名与语义（CPU 秒、loadavg、
// 内存、文件系统、磁盘 IO、网卡流量、启动时间），在 agent 的 mTLS 端口
// 上以 Prometheus 文本格式暴露。刻意不用 node_exporter 全量 collector：
// 只取排障高频子集，Linux 走 /proc + statfs，darwin 演练环境 sysctl 兜底
// （CPU/网卡/磁盘 IO 不可得时输出空集而非报错，与 setup 模块同口径）。
//
// 消费面两个：server 的 scrapeLoop（5 分钟聚合落库供控制台画趋势）与
// Prometheus 直接抓取（server 代理 /api/hosts/{id}/metrics，basic auth）。

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// handleMetrics 输出当前指标快照（Prometheus 文本格式）。
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	start := time.Now()
	body := renderMetrics(collectMetrics())
	// agent 自身采样耗时（毫秒）——排查采集异常用
	body += fmt.Sprintf("wdp_agent_collect_duration_seconds %v\n", time.Since(start).Seconds())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

// sample 是一条指标（名称 + 标签 + 值）。
type sample struct {
	name   string
	labels string // 已渲染好的 {k="v",...}（空 = 无标签）
	value  float64
}

// collector 逐项采集；单项失败输出空（监控不得因部分失败整体 5xx）。
func collectMetrics() []sample {
	var mu sync.Mutex
	out := []sample{}
	addAll := func(ss []sample) {
		mu.Lock()
		out = append(out, ss...)
		mu.Unlock()
	}
	done := make(chan struct{}, 8)
	go func() { addAll(collectCPU()); done <- struct{}{} }()
	go func() { addAll(collectMem()); done <- struct{}{} }()
	go func() { addAll(collectLoad()); done <- struct{}{} }()
	go func() { addAll(collectFilesystem()); done <- struct{}{} }()
	go func() { addAll(collectDiskIO()); done <- struct{}{} }()
	go func() { addAll(collectNet()); done <- struct{}{} }()
	go func() { addAll(collectUptime()); done <- struct{}{} }()
	for i := 0; i < 7; i++ {
		<-done
	}
	out = append(out, sample{name: "node_time_seconds", value: float64(time.Now().Unix())})
	return out
}

func renderMetrics(ss []sample) string {
	var b strings.Builder
	last := ""
	for _, s := range ss {
		if s.name != last {
			b.WriteString("# TYPE " + s.name + " untyped\n")
			last = s.name
		}
		if s.labels == "" {
			fmt.Fprintf(&b, "%s %v\n", s.name, s.value)
		} else {
			fmt.Fprintf(&b, "%s%s %v\n", s.name, s.labels, s.value)
		}
	}
	return b.String()
}

func lbl(kvs ...string) string {
	if len(kvs) == 0 {
		return ""
	}
	var parts []string
	for i := 0; i+1 < len(kvs); i += 2 {
		parts = append(parts, fmt.Sprintf("%s=%q", kvs[i], kvs[i+1]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// ---- CPU：/proc/stat 的 cpuN 行（jiffies / USER_HZ=100 → 秒） ----

const userHZ = 100

func collectCPU() []sample {
	if runtime.GOOS != "linux" {
		return nil
	}
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil
	}
	defer f.Close()
	out := []sample{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		name := fields[0]
		// 只取按核的 cpuN 行；汇总行（cpu）与 intr/ctxt 等其它行跳过
		if !strings.HasPrefix(name, "cpu") || name == "cpu" {
			continue
		}
		cpu := name
		modes := []string{"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal"}
		for i, m := range modes {
			if i >= len(fields)-1 {
				break
			}
			if v, err := strconv.ParseFloat(fields[i+1], 64); err == nil {
				out = append(out, sample{name: "node_cpu_seconds_total", labels: lbl("cpu", cpu[3:], "mode", m), value: v / userHZ})
			}
		}
	}
	return out
}

// ---- 内存：/proc/meminfo（KB → bytes） ----

func collectMem() []sample {
	if runtime.GOOS != "linux" {
		// darwin 兜底：物理内存总量（可用内存无稳定 sysctl，演练只出总量；
		// 内存使用率曲线由 Linux 生产环境提供）
		total, ok := darwinMemTotal()
		if !ok {
			return nil
		}
		return []sample{{name: "node_memory_MemTotal_bytes", value: float64(total)}}
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	var total, avail, swapTotal, swapFree float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(fields[1], 64)
		switch fields[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		case "SwapTotal:":
			swapTotal = v * 1024
		case "SwapFree:":
			swapFree = v * 1024
		}
	}
	out := []sample{}
	if total > 0 {
		out = append(out, sample{name: "node_memory_MemTotal_bytes", value: total})
	}
	if avail > 0 {
		out = append(out, sample{name: "node_memory_MemAvailable_bytes", value: avail})
	}
	if swapTotal > 0 {
		out = append(out,
			sample{name: "node_memory_SwapTotal_bytes", value: swapTotal},
			sample{name: "node_memory_SwapFree_bytes", value: swapFree},
		)
	}
	return out
}

// ---- loadavg ----

func collectLoad() []sample {
	if runtime.GOOS != "linux" {
		// darwin 的 vm.loadavg 是二进制 struct（长度随版本浮动，解析脆弱）；
		// 演练环境不硬解，Linux 主战场不受影响
		return nil
	}
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return nil
	}
	var l1, l5, l15 float64
	if _, err := fmt.Sscanf(string(b), "%f %f %f", &l1, &l5, &l15); err != nil {
		return nil
	}
	return []sample{{name: "node_load1", value: l1}, {name: "node_load5", value: l5}, {name: "node_load15", value: l15}}
}

// ---- 文件系统：真实挂载点的容量/可用/inode ----

var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true,
	"cgroup": true, "cgroup2": true, "securityfs": true, "pstore": true,
	"bpf": true, "tracefs": true, "debugfs": true, "fusectl": true, "configfs": true,
	"autofs": true, "mqueue": true, "hugetlbfs": true, "rpc_pipefs": true,
	"nsfs": true, "squashfs": true, "overlay": true, "iso9660": true,
	"zfs": true, "efivarfs": true, "binfmt_misc": true,
}

func collectFilesystem() []sample {
	type mnt struct{ device, mount, fstype string }
	var mounts []mnt
	if runtime.GOOS == "linux" {
		f, err := os.Open("/proc/self/mounts")
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 64<<10)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 3 || pseudoFS[fields[2]] {
				continue
			}
			mounts = append(mounts, mnt{device: fields[0], mount: fields[1], fstype: fields[2]})
		}
	} else {
		mounts = []mnt{{device: "/dev/root", mount: "/", fstype: "auto"}} // darwin 演练：只看根
	}
	out := []sample{}
	for _, m := range mounts {
		st, ok := fsStats(m.mount)
		if !ok {
			continue
		}
		l := lbl("mount", m.mount, "device", m.device, "fstype", m.fstype)
		out = append(out,
			sample{name: "node_filesystem_size_bytes", labels: l, value: st.sizeBytes},
			sample{name: "node_filesystem_avail_bytes", labels: l, value: st.availBytes},
			sample{name: "node_filesystem_files", labels: l, value: st.files},
			sample{name: "node_filesystem_files_free", labels: l, value: st.filesFree},
		)
	}
	return out
}

// ---- 磁盘 IO：/proc/diskstats（设备级读写字节与繁忙时间） ----

func collectDiskIO() []sample {
	if runtime.GOOS != "linux" {
		return nil
	}
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return nil
	}
	defer f.Close()
	out := []sample{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// 主/次设备号 设备名 完成读 读合并 读扇区 读毫秒 完成写 写合并 写扇区 写毫秒 …
		if len(fields) < 10 {
			continue
		}
		dev := fields[2]
		// 物理设备与分区粒度即可：跳过 dm-/loop/ram（聚合口径交给消费端）
		if strings.HasPrefix(dev, "loop") || strings.HasPrefix(dev, "ram") || strings.HasPrefix(dev, "sr") {
			continue
		}
		readSectors, _ := strconv.ParseFloat(fields[5], 64)
		writeSectors, _ := strconv.ParseFloat(fields[9], 64)
		ioMs, _ := strconv.ParseFloat(fields[12], 64)
		l := lbl("device", dev)
		out = append(out,
			sample{name: "node_disk_read_bytes_total", labels: l, value: readSectors * 512},
			sample{name: "node_disk_written_bytes_total", labels: l, value: writeSectors * 512},
			sample{name: "node_disk_io_time_seconds_total", labels: l, value: ioMs / 1000},
		)
	}
	return out
}

// ---- 网卡：/proc/net/dev（收发字节数，滤虚拟接口） ----

func collectNet() []sample {
	if runtime.GOOS != "linux" {
		return nil
	}
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil
	}
	defer f.Close()
	out := []sample{}
	sc := bufio.NewScanner(f)
	for i := 0; sc.Scan(); i++ {
		if i < 2 { // 两行表头
			continue
		}
		parts := strings.SplitN(sc.Text(), ":", 2)
		if len(parts) != 2 {
			continue
		}
		dev := strings.TrimSpace(parts[0])
		if dev == "lo" || strings.HasPrefix(dev, "veth") || strings.HasPrefix(dev, "docker") || strings.HasPrefix(dev, "br-") {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseFloat(fields[0], 64)
		tx, _ := strconv.ParseFloat(fields[8], 64)
		out = append(out,
			sample{name: "node_network_receive_bytes_total", labels: lbl("device", dev), value: rx},
			sample{name: "node_network_transmit_bytes_total", labels: lbl("device", dev), value: tx},
		)
	}
	return out
}

// ---- 启动时间 ----

func collectUptime() []sample {
	if runtime.GOOS != "linux" {
		// darwin：kern.boottime 是 struct timeval（tv_sec 8 字节小端）
		if sec, ok := darwinBootSecs(); ok && sec > 0 {
			return []sample{{name: "node_boot_time_seconds", value: float64(sec)}}
		}
		return nil
	}
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return nil
	}
	var up float64
	if _, err := fmt.Sscanf(string(b), "%f", &up); err != nil {
		return nil
	}
	return []sample{{name: "node_boot_time_seconds", value: float64(time.Now().Unix()) - up}}
}
