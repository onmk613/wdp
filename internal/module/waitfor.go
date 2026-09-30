package module

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"wdp/internal/i18n"
)

func init() {
	Register(&WaitForModule{})
}

// waitforStates 是 wait_for.state 的合法值域：解析校验与 Params 文档
// 共用同一变量（文档长在实现旁边）。
var waitforStates = []string{"present", "absent"}

// WaitForModule 等待条件满足后放行：TCP 端口可达/释放、远端路径存在/消失。
// 探测在控制端发起（向目标地址 dial、经连接探测远端路径），视角为控制端到目标机地址；
// 模块本身不产生任何变更。
type WaitForModule struct{}

func (m *WaitForModule) Name() string { return "wait_for" }

func (m *WaitForModule) Desc() string {
	return i18n.T("Wait for a port/path condition to hold (polled from the controller; failure on timeout)", "等待端口/路径条件满足（控制端轮询，超时即失败）")
}

func (m *WaitForModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "host", Type: "string", Desc: i18n.T("probe address (defaults to the host Address)", "探测地址（缺省为主机 Address）")},
		{Name: "port", Type: "int", Desc: i18n.T("TCP port: present waits until it is reachable, absent waits until it is closed (mutually exclusive with path)", "TCP 端口：present 等待可连通，absent 等待关闭（与 path 互斥）")},
		{Name: "path", Type: "string", Desc: i18n.T("remote path: present waits for it to appear, absent waits for it to disappear (mutually exclusive with port)", "远端路径：present 等待出现，absent 等待消失（与 port 互斥）")},
		{Name: "state", Type: "string", Default: "present", Enum: waitforStates, Desc: i18n.T("present waits for the condition to hold / absent waits for it to clear", "present 等待条件成立 / absent 等待条件解除")},
		{Name: "timeout", Type: "int", Default: "300", Desc: i18n.T("total seconds to wait (the task fails on timeout)", "总等待秒数（超时任务失败）")},
		{Name: "delay", Type: "int", Default: "0", Desc: i18n.T("seconds to wait before the first probe", "首次探测前等待的秒数")},
		{Name: "sleep", Type: "int", Default: "1", Desc: i18n.T("seconds between two probes", "两次探测间隔秒数")},
		{Name: "msg", Type: "string", Desc: i18n.T("custom message on timeout failure", "超时失败时的自定义消息")},
	}
}

func (m *WaitForModule) Example() string {
	return i18n.T(`- name: wait for the service port to be ready (host defaults to the host address)
  wait_for:
    port: 8080
    delay: 5
    timeout: 120

- name: wait for the remote lock file to disappear
  wait_for:
    path: /var/run/app.lock
    state: absent
    timeout: 60
    msg: app failed to exit in time
`, `- name: 等待服务端口就绪（host 缺省为主机地址）
  wait_for:
    port: 8080
    delay: 5
    timeout: 120

- name: 等待远端锁文件消失
  wait_for:
    path: /var/run/app.lock
    state: absent
    timeout: 60
    msg: app failed to exit in time
`)
}

// minPollInterval 是轮询探测的最小间隔（见 poll 的轮询循环）。
const minPollInterval = time.Second

// probeDialTimeout 是单次 TCP 探测的拨号超时：探测失败仅代表"当下不可达"
// （由轮询重试），过长则单轮探测就能占满整个等待窗口。
const probeDialTimeout = time.Second

// waitForReq 是 wait_for 解析后的参数。
type waitForReq struct {
	host       string
	path       string
	hasPath    bool
	absent     bool // state=absent：等待条件消除而非达成
	port       int
	timeoutSec int
	delaySec   int
	sleepSec   int
	msg        string // 自定义超时失败文案
	desc       string // 展示用条件描述（host:port 或 path）
	readyWord  string // 条件达成时的措辞
	goneWord   string // 条件消除时的措辞
}

// parseWaitForArgs 解析并校验 wait_for 参数（host 缺省取主机地址/名、
// port 范围与 port/path 互斥、timeout/delay/sleep 非负）。
func parseWaitForArgs(rc *RunContext, args map[string]any) (*waitForReq, *Result) {
	host, _ := argStr(args, "host")
	if host == "" {
		host = rc.Host.Address
		if host == "" {
			host = rc.Host.Name
		}
	}
	path, hasPath := argStr(args, "path")
	state, ok := parseState(args, "present", waitforStates...)
	if !ok {
		return nil, Fail("unsupported state %q (options: %s)", state, strings.Join(waitforStates, "/"))
	}
	w := &waitForReq{host: host, path: path, hasPath: hasPath, absent: state == "absent"}

	if s, ok := argStr(args, "port"); ok && strings.TrimSpace(s) != "" {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 || n > 65535 {
			return nil, Fail("port must be an integer between 1 and 65535")
		}
		w.port = n
	}
	if w.port == 0 && !hasPath {
		return nil, Fail("wait_for requires a port or path parameter")
	}
	if w.port != 0 && hasPath {
		return nil, Fail("port and path are mutually exclusive")
	}
	if hasPath && path == "" {
		return nil, Fail("path must not be empty")
	}

	timeoutSec, ok := argSecs(args, "timeout", 300)
	if !ok || timeoutSec <= 0 {
		return nil, Fail("timeout must be a positive integer")
	}
	delaySec, ok := argSecs(args, "delay", 0)
	if !ok {
		return nil, Fail("delay must be a non-negative integer")
	}
	sleepSec, ok := argSecs(args, "sleep", 1)
	if !ok || sleepSec < 0 {
		return nil, Fail("sleep must be a non-negative integer")
	}
	w.timeoutSec, w.delaySec, w.sleepSec = timeoutSec, delaySec, sleepSec
	w.msg, _ = argStr(args, "msg")

	w.desc = path
	w.readyWord, w.goneWord = "ready", "removed"
	if w.port != 0 {
		w.desc = net.JoinHostPort(host, strconv.Itoa(w.port))
		w.goneWord = "closed"
	}
	return w, nil
}

// Run 执行等待：解析 → check 单次探测 / 轮询等待（骨架与 user 模块一致）。
func (m *WaitForModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	w, bad := parseWaitForArgs(rc, args)
	if bad != nil {
		return bad
	}
	// check 模式：单次只读探测报告当前状态，不等待
	if rc.CheckMode {
		return m.checkProbe(rc, w)
	}
	return m.poll(rc, w)
}

// checkProbe 单次只读探测并报告当前状态（check 模式，不等待）。
func (m *WaitForModule) checkProbe(rc *RunContext, w *waitForReq) *Result {
	ok, bad := m.probe(rc, w.host, w.port, w.path, w.absent)
	if bad != nil {
		return bad
	}
	label := "not ready"
	if ok {
		label = w.readyWord
		if w.absent {
			label = w.goneWord
		}
	}
	return &Result{Msg: fmt.Sprintf("[check] %s currently %s (single probe, no waiting)", w.desc, label)}
}

// poll 轮询等待条件满足：delay 先行，探测-休眠循环收口到 deadline，
// 超时（或 ctx 取消）失败。
func (m *WaitForModule) poll(rc *RunContext, w *waitForReq) *Result {
	// 轮询窗口：timeout 与任务级超时（rc.TimeoutMs）取较小值
	deadline := time.Now().Add(time.Duration(w.timeoutSec) * time.Second)
	if rc.TimeoutMs > 0 {
		if t := time.Now().Add(time.Duration(rc.TimeoutMs) * time.Millisecond); t.Before(deadline) {
			deadline = t
		}
	}
	if w.delaySec > 0 && !waitInterruptible(rc.Ctx, time.Duration(w.delaySec)*time.Second) {
		return Fail("wait_for cancelled: %v", rc.Ctx.Err())
	}

	start := time.Now()
	for {
		ok, bad := m.probe(rc, w.host, w.port, w.path, w.absent)
		if bad != nil {
			return bad
		}
		if ok {
			word := w.readyWord
			if w.absent {
				word = w.goneWord
			}
			return &Result{Msg: fmt.Sprintf("%s %s (%.0fs)", w.desc, word, time.Since(start).Seconds())}
		}
		now := time.Now()
		if !now.Before(deadline) {
			break
		}
		wait := time.Duration(w.sleepSec) * time.Second
		if wait < minPollInterval {
			// sleep: 0 合法，但轮询间隔会退化成探测本身的耗时：path 探测
			// 每轮一次远端 exec，无间隔即高频轰炸连接层（连接复用与并发
			// 审计都会把放大效应叠上去）。轮询设最小间隔；sleep >= 1s 时
			// 尊重用户值。
			wait = minPollInterval
		}
		if now.Add(wait).After(deadline) {
			wait = deadline.Sub(now) // 收口到 deadline，避免超出 timeout
		}
		if !waitInterruptible(rc.Ctx, wait) {
			return Fail("wait_for cancelled: %v", rc.Ctx.Err())
		}
	}

	base := w.msg
	if base == "" {
		base = fmt.Sprintf("timed out waiting for %s", w.desc)
	}
	return &Result{Failed: true, Msg: fmt.Sprintf("%s (timeout %ds)", base, w.timeoutSec)}
}

// probe 单次探测条件是否满足（ok=条件达成）。
func (m *WaitForModule) probe(rc *RunContext, host string, port int, path string, absent bool) (bool, *Result) {
	if port != 0 {
		d := net.Dialer{Timeout: probeDialTimeout}
		conn, err := d.DialContext(rc.Ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return absent, nil
		}
		conn.Close()
		return !absent, nil
	}
	kind, bad := probePath(rc, path)
	if bad != nil {
		return false, bad
	}
	exists := kind != "missing"
	return exists != absent, nil
}

// waitInterruptible 可中断等待（ctx 取消返回 false）。
func waitInterruptible(ctx context.Context, d time.Duration) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	timer := time.NewTimer(d)
	defer timer.Stop() // ctx 提前取消时立即释放定时器（time.After 需等到期才回收）
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// argSecs 解析秒数参数（非负整数；缺省返回 def，非法返回 ok=false）。
func argSecs(args map[string]any, key string, def int) (int, bool) {
	s, ok := argStr(args, key)
	if !ok || strings.TrimSpace(s) == "" {
		return def, true
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
