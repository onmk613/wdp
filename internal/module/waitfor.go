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

// WaitForModule 等待条件满足后放行：TCP 端口可达/释放、远端路径存在/消失。
// 探测在控制端发起（向目标地址 dial、经连接探测远端路径），视角为控制端到目标机地址；
// 模块本身不产生任何变更。
type WaitForModule struct{}

// Name 模块名。
func (m *WaitForModule) Name() string { return "wait_for" }

// Desc 模块说明。
func (m *WaitForModule) Desc() string {
	return i18n.T("wait until a port/path condition is met (polled from the controller)", "等待端口/路径条件满足（控制端视角轮询）")
}

// Params 参数文档。
func (m *WaitForModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "host", Type: "string", Desc: "探测地址（缺省用主机 Address）"},
		{Name: "port", Type: "int", Desc: "TCP 端口：state=present 等待可达，absent 等待关闭（与 path 二选一）"},
		{Name: "path", Type: "string", Desc: "远端路径：state=present 等待存在，absent 等待消失（与 port 二选一）"},
		{Name: "state", Type: "string", Default: "present", Desc: "present 条件出现 / absent 条件消失"},
		{Name: "timeout", Type: "int", Default: "300", Desc: "总等待秒数（超时判失败）"},
		{Name: "delay", Type: "int", Default: "0", Desc: "首次探测前等待秒数"},
		{Name: "sleep", Type: "int", Default: "1", Desc: "两次探测间隔秒数"},
		{Name: "msg", Type: "string", Desc: "超时失败时的自定义消息"},
	}
}

// Example 示例任务。
func (m *WaitForModule) Example() string {
	return `- name: 等待服务端口就绪（host 缺省用主机地址）
  wait_for:
    port: 8080
    delay: 5
    timeout: 120

- name: 等待远端锁文件消失
  wait_for:
    path: /var/run/app.lock
    state: absent
    timeout: 60
    msg: 应用未能及时退出
`
}

// Run 执行等待。
func (m *WaitForModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	host, _ := argStr(args, "host")
	if host == "" {
		host = rc.Host.Address
		if host == "" {
			host = rc.Host.Name
		}
	}
	path, hasPath := argStr(args, "path")
	state, _ := argStr(args, "state")
	switch state {
	case "", "present", "absent":
	default:
		return Fail("不支持的 state %q（可选: present/absent）", state)
	}
	absent := state == "absent"

	var port int
	if s, ok := argStr(args, "port"); ok && strings.TrimSpace(s) != "" {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 || n > 65535 {
			return Fail("port 应为 1-65535 的整数")
		}
		port = n
	}
	if port == 0 && !hasPath {
		return Fail("wait_for 需要 port 或 path 参数")
	}
	if port != 0 && hasPath {
		return Fail("port 与 path 只能二选一")
	}
	if hasPath && path == "" {
		return Fail("path 不能为空")
	}

	timeoutSec, ok := argSecs(args, "timeout", 300)
	if !ok || timeoutSec <= 0 {
		return Fail("timeout 应为正整数")
	}
	delaySec, ok := argSecs(args, "delay", 0)
	if !ok {
		return Fail("delay 应为非负整数")
	}
	sleepSec, ok := argSecs(args, "sleep", 1)
	if !ok || sleepSec < 0 {
		return Fail("sleep 应为非负整数")
	}
	customMsg, _ := argStr(args, "msg")

	desc := path
	readyWord, goneWord := "就绪", "已移除"
	if port != 0 {
		desc = net.JoinHostPort(host, strconv.Itoa(port))
		goneWord = "已关闭"
	}

	// check 模式：单次只读探测报告当前状态，不等待
	if rc.CheckMode {
		ok, bad := m.probe(rc, host, port, path, absent)
		if bad != nil {
			return bad
		}
		label := "未就绪"
		if ok {
			label = readyWord
			if absent {
				label = goneWord
			}
		}
		return &Result{Msg: fmt.Sprintf("[check] %s 当前%s（单次探测，不等待）", desc, label)}
	}

	// 轮询窗口：timeout 与任务级超时（rc.TimeoutMs）取较小值
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	if rc.TimeoutMs > 0 {
		if t := time.Now().Add(time.Duration(rc.TimeoutMs) * time.Millisecond); t.Before(deadline) {
			deadline = t
		}
	}
	if delaySec > 0 && !waitInterruptible(rc.Ctx, time.Duration(delaySec)*time.Second) {
		return Fail("wait_for 被取消: %v", rc.Ctx.Err())
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
			return &Result{Msg: fmt.Sprintf("%s %s（%.0f 秒）", desc, word, time.Since(start).Seconds())}
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
			return Fail("wait_for 被取消: %v", rc.Ctx.Err())
		}
	}

	base := customMsg
	if base == "" {
		base = fmt.Sprintf("等待 %s 超时", desc)
	}
	return &Result{Failed: true, Msg: fmt.Sprintf("%s（超时 %d 秒）", base, timeoutSec)}
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
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
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
