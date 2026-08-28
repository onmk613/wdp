package module

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

func init() {
	Register(&WaitForModule{})
}

// WaitForModule 等待条件满足后放行：TCP 端口可达/释放、远端路径存在/消失。
// 探测在控制端发起（向目标地址 dial、经连接探测远端路径），视角为控制端到目标机地址；
// 模块本身不产生任何变更。
type WaitForModule struct{}

// Name 模块名。
func (m *WaitForModule) Name() string { return "wait_for" }

// Desc 模块说明。
func (m *WaitForModule) Desc() string {
	return "wait until a port/path condition is met (polled from the controller)"
}

// Params 参数文档。
func (m *WaitForModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "host", Type: "string", Desc: "probe address (defaults to the host Address)"},
		{Name: "port", Type: "int", Desc: "TCP port: state=present waits for reachable, absent waits for closed (mutually exclusive with path)"},
		{Name: "path", Type: "string", Desc: "remote path: state=present waits for existence, absent waits for removal (mutually exclusive with port)"},
		{Name: "state", Type: "string", Default: "present", Desc: "present waits for the condition / absent waits for it to clear"},
		{Name: "timeout", Type: "int", Default: "300", Desc: "total wait seconds (timeout fails the task)"},
		{Name: "delay", Type: "int", Default: "0", Desc: "seconds to wait before the first probe"},
		{Name: "sleep", Type: "int", Default: "1", Desc: "seconds between probes"},
		{Name: "msg", Type: "string", Desc: "custom message on timeout failure"},
	}
}

// Example 示例任务。
func (m *WaitForModule) Example() string {
	return `- name: wait for the service port (host defaults to the host address)
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
`
}

// Run 执行等待。
func (m *WaitForModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	host, _ := argStr(args, "host")
	if host == "" {
		host = rc.Host.Address
		if host == "" {
			host = rc.Host.Name
		}
	}
	path, hasPath := argStr(args, "path")
	state, ok := parseState(args, "present", "present", "absent")
	if !ok {
		return Fail("unsupported state %q (options: present/absent)", state)
	}
	absent := state == "absent"

	var port int
	if s, ok := argStr(args, "port"); ok && strings.TrimSpace(s) != "" {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 || n > 65535 {
			return Fail("%s", "port must be an integer between 1 and 65535")
		}
		port = n
	}
	if port == 0 && !hasPath {
		return Fail("%s", "wait_for requires a port or path parameter")
	}
	if port != 0 && hasPath {
		return Fail("%s", "port and path are mutually exclusive")
	}
	if hasPath && path == "" {
		return Fail("%s", "path must not be empty")
	}

	timeoutSec, ok := argSecs(args, "timeout", 300)
	if !ok || timeoutSec <= 0 {
		return Fail("%s", "timeout must be a positive integer")
	}
	delaySec, ok := argSecs(args, "delay", 0)
	if !ok {
		return Fail("%s", "delay must be a non-negative integer")
	}
	sleepSec, ok := argSecs(args, "sleep", 1)
	if !ok || sleepSec < 0 {
		return Fail("%s", "sleep must be a non-negative integer")
	}
	customMsg, _ := argStr(args, "msg")

	desc := path
	readyWord, goneWord := "ready", "removed"
	if port != 0 {
		desc = net.JoinHostPort(host, strconv.Itoa(port))
		goneWord = "closed"
	}

	// check 模式：单次只读探测报告当前状态，不等待
	if rc.CheckMode {
		ok, bad := m.probe(rc, host, port, path, absent)
		if bad != nil {
			return bad
		}
		label := "not ready"
		if ok {
			label = readyWord
			if absent {
				label = goneWord
			}
		}
		return &Result{Msg: fmt.Sprintf("[check] %s currently %s (single probe, no waiting)", desc, label)}
	}

	// 轮询窗口：timeout 与任务级超时（rc.TimeoutMs）取较小值
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	if rc.TimeoutMs > 0 {
		if t := time.Now().Add(time.Duration(rc.TimeoutMs) * time.Millisecond); t.Before(deadline) {
			deadline = t
		}
	}
	if delaySec > 0 && !waitInterruptible(rc.Ctx, time.Duration(delaySec)*time.Second) {
		return Fail("wait_for cancelled: %v", rc.Ctx.Err())
	}

	start := time.Now()
	for {
		ok, bad := m.probe(rc, host, port, path, absent)
		if bad != nil {
			return bad
		}
		if ok {
			word := readyWord
			if absent {
				word = goneWord
			}
			return &Result{Msg: fmt.Sprintf("%s %s (%.0fs)", desc, word, time.Since(start).Seconds())}
		}
		now := time.Now()
		if !now.Before(deadline) {
			break
		}
		wait := time.Duration(sleepSec) * time.Second
		if now.Add(wait).After(deadline) {
			wait = deadline.Sub(now) // 收口到 deadline，避免超出 timeout
		}
		if !waitInterruptible(rc.Ctx, wait) {
			return Fail("wait_for cancelled: %v", rc.Ctx.Err())
		}
	}

	base := customMsg
	if base == "" {
		base = fmt.Sprintf("timed out waiting for %s", desc)
	}
	return &Result{Failed: true, Msg: fmt.Sprintf("%s (timeout %ds)", base, timeoutSec)}
}

// probe 单次探测条件是否满足（ok=条件达成）。
func (m *WaitForModule) probe(rc *RunContext, host string, port int, path string, absent bool) (bool, *Result) {
	if port != 0 {
		d := net.Dialer{Timeout: time.Second}
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
