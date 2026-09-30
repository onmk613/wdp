package module

// get_url 远端模式（via 缺省=remote）：脚本形态与真实行为。行为测试用
// local 连接（真 sh）+ 本地 httptest 服务器端到端跑通：目标机直接下载、
// sha256 校验、幂等短路、校验失败不落盘。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/conn"
	_ "wdp/internal/conn/local" // 注册 local 连接工厂（端到端用真 sh 执行）
	"wdp/internal/model"
)

// TestGetURLRemoteIsDefaultAndScriptShape 缺省即 remote；脚本要素：
// curl/wget 双分支、URL 单引号字面量（防注入）、sha256 校验与 42 哨兵、
// 原子 mv、--max-filesize 上限注入。
func TestGetURLRemoteIsDefaultAndScriptShape(t *testing.T) {
	rc, fake := newTestRC(t)
	var script string
	fake.ExecFn = func(req conn.ExecRequest) (conn.ExecResult, error) {
		script = req.Script
		return conn.ExecResult{Code: 0, Stdout: "wdp_sum=abc\n"}, nil
	}
	r := (&GetURLModule{}).Run(rc, map[string]any{
		"url": "https://example.com/app';reboot;'.tgz", "dest": "/opt/app.tgz",
		"sha256:": strings.Repeat("a", 64), // 非法键占位，不影响
		"sha256":  strings.Repeat("a", 64),
		"mode":    "0755",
	}, "")
	if r.Failed {
		t.Fatalf("remote 下载失败: %s", r.Msg)
	}
	if !strings.Contains(script, "curl -fsSL") || !strings.Contains(script, "wget -q") {
		t.Fatalf("脚本应有 curl/wget 双分支:\n%s", script)
	}
	// shellquote 转义形态存在 = URL 按单引号字面量下发（注入面封死）
	if !strings.Contains(script, `'\'';reboot;'\''`) {
		t.Fatalf("URL 应单引号字面量化（注入面）:\n%s", script)
	}
	if !strings.Contains(script, "exit 42") {
		t.Fatalf("sha256 校验失败应走 42 哨兵:\n%s", script)
	}
	if !strings.Contains(script, "mv -f \"$tmp\"") {
		t.Fatalf("应原子 mv:\n%s", script)
	}
	if !strings.Contains(script, "--max-filesize") {
		t.Fatalf("应注入下载上限:\n%s", script)
	}
}

// TestGetURLRemoteEndToEnd local 连接（真 sh）+ httptest：真实下载 →
// sha256 校验 → 幂等 → 校验失败不落盘。
func TestGetURLRemoteEndToEnd(t *testing.T) {
	const body = "remote-dl-bytes-v1"
	srv := newHTTPServer(t, body)
	defer srv.Close()
	sum := sha256hex([]byte(body))
	dest := filepath.Join(t.TempDir(), "app.tgz")

	lc, lerr := conn.NewConnection(&model.Host{Name: "local-test", Conn: "local"}, nil)
	if lerr != nil {
		t.Fatal(lerr)
	}
	rc := &RunContext{
		Ctx:  context.Background(),
		Conn: lc,
		Host: &model.Host{Name: "local-test"},
		Vars: map[string]any{},
	}
	mod := &GetURLModule{}

	r1 := mod.Run(rc, map[string]any{"url": srv.URL + "/a.tgz", "dest": dest, "sha256": sum, "mode": "0755"}, "")
	if r1.Failed {
		t.Fatalf("远端下载失败: %s", r1.Msg)
	}
	if !r1.Changed {
		t.Fatal("首次下载应 changed")
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != body {
		t.Fatalf("下载内容异常: %v %q", err, data)
	}
	if st, _ := os.Stat(dest); st != nil && st.Mode().Perm() != 0o755 {
		t.Fatalf("权限应 0755: %v", st.Mode())
	}

	// 幂等：sha256 已一致 → 跳过、不变更（mode 同值，否则属权限修正语义）
	r2 := mod.Run(rc, map[string]any{"url": srv.URL + "/a.tgz", "dest": dest, "sha256": sum, "mode": "0755"}, "")
	if r2.Failed || r2.Changed {
		t.Fatalf("二次执行应跳过: %+v", r2)
	}

	// 校验失败：不落盘
	badDest := filepath.Join(t.TempDir(), "bad.tgz")
	srv2 := newHTTPServer(t, "other-content")
	defer srv2.Close()
	r3 := mod.Run(rc, map[string]any{"url": srv2.URL + "/b.tgz", "dest": badDest, "sha256": sum}, "")
	if !r3.Failed || !strings.Contains(r3.Msg, "checksum mismatch") {
		t.Fatalf("校验失败应报错: %+v", r3)
	}
	if _, err := os.Stat(badDest); !os.IsNotExist(err) {
		t.Fatalf("校验失败不应落盘: %v", err)
	}

	// 404：下载失败、目标不落盘
	srv3 := newHTTPServer(t, "x")
	defer srv3.Close()
	r4 := mod.Run(rc, map[string]any{"url": srv3.URL + "/missing", "dest": badDest}, "")
	if !r4.Failed || !strings.Contains(r4.Msg, "remote download failed") {
		t.Fatalf("404 应失败: %+v", r4)
	}
}
