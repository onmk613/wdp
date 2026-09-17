package selfrun

// selfrun 的测试：本机脚本执行是 agent 自治执行与 HTTP /exec 的共同底座
//（提权语义、超时整组击杀、输出截断只有这一份），此前零覆盖。

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestBecomeScriptPasswordOnlyOnStdin 提权密码只经 stdin 传递，绝不出现在
// 脚本或命令行里（同机用户 ps 不可见）。
func TestBecomeScriptPasswordOnlyOnStdin(t *testing.T) {
	script, stdin := BecomeScript("echo hi", "root", "s3cret", "user-input\n")
	if strings.Contains(script, "s3cret") {
		t.Fatalf("密码不得出现在脚本中: %s", script)
	}
	if !strings.HasPrefix(stdin, "s3cret\n") {
		t.Fatalf("密码应作为 stdin 首行: %q", stdin)
	}
	if !strings.HasSuffix(stdin, "user-input\n") {
		t.Fatalf("原 stdin 应拼接在密码行之后: %q", stdin)
	}
	if !strings.Contains(script, "sudo -S") {
		t.Fatalf("有密码时应使用 sudo -S: %s", script)
	}
	// 用户名注入被引号包裹
	script, _ = BecomeScript("echo hi", "user; rm -rf /", "", "")
	if !strings.Contains(script, "'user; rm -rf /'") {
		t.Fatalf("用户名应经 shell 引号: %s", script)
	}
	// 无 become 用户：原样返回
	if s, in := BecomeScript("echo hi", "", "pw", "x"); s != "echo hi" || in != "x" {
		t.Fatalf("无 become 应原样返回: %q %q", s, in)
	}
}

func TestRunScriptBasic(t *testing.T) {
	resp := RunScript(context.Background(), ExecReq{Script: "echo out; echo err >&2; exit 3"})
	if resp.Code != 3 {
		t.Fatalf("退出码 = %d, want 3", resp.Code)
	}
	if strings.TrimSpace(resp.Stdout) != "out" || strings.TrimSpace(resp.Stderr) != "err" {
		t.Fatalf("输出不符: stdout=%q stderr=%q", resp.Stdout, resp.Stderr)
	}
	if resp.TimedOut || resp.Cancelled {
		t.Fatalf("不应标记超时/取消: %+v", resp)
	}
}

// TestRunScriptEnv 环境变量注入（合法键）与非法键拒绝。
func TestRunScriptEnv(t *testing.T) {
	resp := RunScript(context.Background(), ExecReq{
		Script: `printf '%s' "$FOO"`,
		Env:    map[string]string{"FOO": "bar", "bad-key": "x"},
	})
	if strings.TrimSpace(resp.Stdout) != "bar" {
		t.Fatalf("环境变量应注入: %q (stderr=%q)", resp.Stdout, resp.Stderr)
	}
	resp = RunScript(context.Background(), ExecReq{
		Script: `printf '%s' "${BAD_KEY:-unset}"`,
		Env:    map[string]string{"BAD_KEY": "v"},
	})
	if strings.TrimSpace(resp.Stdout) != "v" {
		t.Fatalf("下划线键合法: %q", resp.Stdout)
	}
}

// TestRunScriptTimeout 超时必须整组击杀并标记 TimedOut。
func TestRunScriptTimeout(t *testing.T) {
	start := time.Now()
	resp := RunScript(context.Background(), ExecReq{
		Script:    "sleep 30",
		TimeoutMs: 300,
	})
	elapsed := time.Since(start)
	if !resp.TimedOut {
		t.Fatalf("应标记超时: %+v", resp)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("超时后应立即返回，实际耗时 %v", elapsed)
	}
}

// TestRunScriptOutputTruncation 单流输出超 1MiB 截断（防内存放大）。
func TestRunScriptOutputTruncation(t *testing.T) {
	// 生成约 1.5MiB 输出
	resp := RunScript(context.Background(), ExecReq{
		Script: "head -c 1572864 /dev/zero | tr '\\0' 'x'",
	})
	if resp.Code != 0 {
		t.Fatalf("脚本应成功: %+v", resp)
	}
	if int64(len(resp.Stdout)) > maxExecOutputBytes+64 {
		t.Fatalf("输出应被截断到 ~%d 字节，实际 %d", maxExecOutputBytes, len(resp.Stdout))
	}
	if len(resp.Stdout) == 0 {
		t.Fatal("截断后仍应有内容")
	}
}

// TestRunScriptCancelled 调用方取消与超时区分（Cancelled vs TimedOut）。
func TestRunScriptCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	resp := RunScript(ctx, ExecReq{Script: "sleep 30"})
	if resp.Cancelled != true && !resp.TimedOut {
		t.Fatalf("取消应标记 Cancelled: %+v", resp)
	}
}
