package selfrun

// selfrun 的测试：本机脚本执行是 agent 自治执行与 HTTP /exec 的共同底座
//（提权语义、超时整组击杀、输出截断只有这一份），此前零覆盖。

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestBecomeCmdPasswordOnlyOnStdin 提权密码只经 stdin 传递，绝不进 argv；
// 脚本体以临时文件路径引用，不内联在命令行（同机用户 ps 不可见）。
func TestBecomeCmdPasswordOnlyOnStdin(t *testing.T) {
	argv, stdin := becomeCmd("/tmp/.wdp-exec-x", "root", "s3cret", "user-input\n")
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "s3cret") {
		t.Fatalf("密码不得出现在 argv: %s", joined)
	}
	if !strings.HasPrefix(stdin, "s3cret\n") {
		t.Fatalf("密码应作为 stdin 首行: %q", stdin)
	}
	if !strings.HasSuffix(stdin, "user-input\n") {
		t.Fatalf("原 stdin 应拼接在密码行之后: %q", stdin)
	}
	if !strings.Contains(joined, "sudo -S") {
		t.Fatalf("有密码时应使用 sudo -S: %s", joined)
	}
	// argv 引用脚本文件路径而非脚本体
	if !strings.Contains(joined, "/tmp/.wdp-exec-x") {
		t.Fatalf("argv 应引用脚本文件路径: %s", joined)
	}
	// 免密 become：sudo -n
	argv, _ = becomeCmd("/tmp/.wdp-exec-x", "appuser", "", "")
	if !strings.Contains(strings.Join(argv, " "), "sudo -n") {
		t.Fatalf("无密码时应使用 sudo -n: %v", argv)
	}
}

// TestRunScriptNotInArgv 脚本体不得进 argv（回归：同机任意用户可经
// ps//proc/<pid>/cmdline 看到任意进程的完整命令行，脚本内嵌的
// export TOKEN=…/口令会随 argv 广播）。观察法：脚本自己 ps 父进程
// （即执行脚本的 sh）并在其命令行里找脚本体标记——内联实现会命中 1。
func TestRunScriptNotInArgv(t *testing.T) {
	resp := RunScript(context.Background(), ExecReq{
		Script: `ps -o command= -p $PPID | grep -c wdp-argv-marker-9f3a`,
	})
	if resp.Code != 1 { // grep 无命中退出 1；若脚本进了 argv 会命中、退出 0
		t.Fatalf("脚本体不得出现在父进程命令行: %+v (stdout=%q)", resp, resp.Stdout)
	}
	if strings.TrimSpace(resp.Stdout) != "0" {
		t.Fatalf("ps 中不应找到脚本体标记: %q", resp.Stdout)
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

// TestPrepareScriptEnvBecomeKeyWhitelist 回归：become 路径的 env 键白名单
// 曾与 sshc/local 各自为政（selfrun 漏下划线续位），FOO_BAR 经 agent 通道
// become 时被静默丢弃而 SSH 通道正常——同名任务跨通道行为分叉。白名单
// 已收敛到 conn.EnvKeyAllowed 单一实现，本测试锁定 become 路径的口径。
func TestPrepareScriptEnvBecomeKeyWhitelist(t *testing.T) {
	script, env := prepareScriptEnv(ExecReq{
		Script:     ":",
		BecomeUser: "root",
		Env:        map[string]string{"FOO_BAR": "v1", "_LEAD": "v2", "bad-key": "x", "A B": "y"},
	})
	if !strings.Contains(script, "export FOO_BAR='v1'") {
		t.Fatalf("下划线键在 become 路径不得丢弃: %q", script)
	}
	if !strings.Contains(script, "export _LEAD='v2'") {
		t.Fatalf("下划线前导键合法: %q", script)
	}
	if strings.Contains(script, "bad-key") || strings.Contains(script, "A B") {
		t.Fatalf("越白名单的键必须丢弃: %q", script)
	}
	if strings.Contains(strings.Join(env, "\n"), "FOO_BAR") {
		t.Fatalf("become 时 env 应写进脚本体而非进程环境: %v", env)
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
