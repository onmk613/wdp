package module

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// closedTCPPort 找一个必然关闭的本地端口（先监听再关闭）。
func closedTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func TestWaitForPortReady(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	rc, _ := newTestRC(t)
	mod := &WaitForModule{}
	r := mod.Run(rc, map[string]any{"host": "127.0.0.1", "port": port}, "")
	if r.Failed {
		t.Fatalf("端口已监听应就绪: %s", r.Msg)
	}
	if r.Changed {
		t.Fatal("wait_for 不产生变更")
	}
	if !strings.Contains(r.Msg, "就绪") {
		t.Fatalf("msg: %s", r.Msg)
	}
}

func TestWaitForPortAbsentReady(t *testing.T) {
	port := closedTCPPort(t)
	rc, _ := newTestRC(t)
	mod := &WaitForModule{}
	r := mod.Run(rc, map[string]any{"host": "127.0.0.1", "port": port, "state": "absent", "timeout": 3}, "")
	if r.Failed {
		t.Fatalf("端口已关闭应立即满足: %s", r.Msg)
	}
	if !strings.Contains(r.Msg, "已关闭") {
		t.Fatalf("msg: %s", r.Msg)
	}
}

func TestWaitForPortTimeout(t *testing.T) {
	port := closedTCPPort(t)
	rc, _ := newTestRC(t)
	mod := &WaitForModule{}
	start := time.Now()
	r := mod.Run(rc, map[string]any{
		"host": "127.0.0.1", "port": port, "timeout": 1, "sleep": 1, "msg": "服务未起来",
	}, "")
	if !r.Failed {
		t.Fatal("端口持续不可达应超时失败")
	}
	if !strings.Contains(r.Msg, "服务未起来") || !strings.Contains(r.Msg, "超时") {
		t.Fatalf("超时消息: %s", r.Msg)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("应遵守 timeout（实际 %v）", d)
	}
}

func TestWaitForHostDefault(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	rc, _ := newTestRC(t)
	rc.Host.Address = "127.0.0.1" // 缺省 host 取主机地址
	mod := &WaitForModule{}
	r := mod.Run(rc, map[string]any{"port": port}, "")
	if r.Failed {
		t.Fatalf("缺省 host 应取主机地址: %s", r.Msg)
	}
}

func TestWaitForPathStates(t *testing.T) {
	rc, fake := newTestRC(t)
	fake.Files["/data/ready"] = []byte("ok")
	mod := &WaitForModule{}

	// present：路径存在 → 就绪
	if r := mod.Run(rc, map[string]any{"path": "/data/ready"}, ""); r.Failed {
		t.Fatalf("路径存在应就绪: %s", r.Msg)
	}
	// absent：路径存在 → 超时失败（自定义 msg 透出）
	r := mod.Run(rc, map[string]any{"path": "/data/ready", "state": "absent", "timeout": 1, "sleep": 1, "msg": "服务未退出"}, "")
	if !r.Failed || !strings.Contains(r.Msg, "服务未退出") {
		t.Fatalf("路径未消失应超时: %+v", r)
	}
	// absent：路径不存在 → 立即满足
	if r := mod.Run(rc, map[string]any{"path": "/data/lock", "state": "absent"}, ""); r.Failed {
		t.Fatalf("路径不存在应满足: %s", r.Msg)
	}
	// present：路径不存在 → 超时失败
	if r := mod.Run(rc, map[string]any{"path": "/data/lock", "timeout": 1, "sleep": 1}, ""); !r.Failed {
		t.Fatal("路径缺失应超时失败")
	}
}

func TestWaitForCheckModeNoWait(t *testing.T) {
	port := closedTCPPort(t)
	rc, _ := newTestRC(t)
	rc.CheckMode = true
	mod := &WaitForModule{}

	start := time.Now()
	r := mod.Run(rc, map[string]any{"host": "127.0.0.1", "port": port, "timeout": 300}, "")
	if r.Failed {
		t.Fatalf("check 只报告状态不应失败: %s", r.Msg)
	}
	if !strings.Contains(r.Msg, "[check]") || !strings.Contains(r.Msg, "未就绪") {
		t.Fatalf("check 报告: %s", r.Msg)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("check 模式不应等待（实际 %v）", d)
	}

	// 就绪侧：真监听 → 报告就绪
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	r = mod.Run(rc, map[string]any{"host": "127.0.0.1", "port": ln.Addr().(*net.TCPAddr).Port}, "")
	if r.Failed || !strings.Contains(r.Msg, "就绪") {
		t.Fatalf("check 就绪报告: %+v", r)
	}
}

func TestWaitForContextCancel(t *testing.T) {
	port := closedTCPPort(t)
	rc, _ := newTestRC(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 进入即取消
	rc.Ctx = ctx
	mod := &WaitForModule{}
	start := time.Now()
	r := mod.Run(rc, map[string]any{"host": "127.0.0.1", "port": port, "timeout": 30, "sleep": 1}, "")
	if !r.Failed || !strings.Contains(r.Msg, "取消") {
		t.Fatalf("ctx 取消应失败: %+v", r)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("取消应立即生效（实际 %v）", d)
	}
}

func TestWaitForArgValidation(t *testing.T) {
	rc, _ := newTestRC(t)
	mod := &WaitForModule{}
	if r := mod.Run(rc, nil, ""); !r.Failed {
		t.Fatal("缺 port/path 应失败")
	}
	if r := mod.Run(rc, map[string]any{"port": "abc"}, ""); !r.Failed {
		t.Fatal("非法 port 应失败")
	}
	if r := mod.Run(rc, map[string]any{"port": 70000}, ""); !r.Failed {
		t.Fatal("port 越界应失败")
	}
	if r := mod.Run(rc, map[string]any{"port": "80", "path": "/x"}, ""); !r.Failed {
		t.Fatal("port 与 path 同给应失败")
	}
	if r := mod.Run(rc, map[string]any{"path": "/x", "state": "weird"}, ""); !r.Failed {
		t.Fatal("非法 state 应失败")
	}
	if r := mod.Run(rc, map[string]any{"path": "/x", "timeout": -1}, ""); !r.Failed {
		t.Fatal("非法 timeout 应失败")
	}
}
