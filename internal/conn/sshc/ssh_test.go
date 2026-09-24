package sshc

import (
	"bytes"
	"encoding/base64"
	"os/exec"
	"strings"
	"testing"

	"wdp/internal/conn"
)

func TestWrapScriptPlain(t *testing.T) {
	req := conn.ExecRequest{Script: "echo hi"}
	s := WrapScript(req, "")
	// 脚本体不得出现在 argv（ps / /proc/*/cmdline 对同机用户可见）
	if strings.Contains(s, base64.StdEncoding.EncodeToString([]byte("echo hi"))) {
		t.Fatalf("脚本体不得拼进命令行: %s", s)
	}
	if !strings.Contains(s, `sh "$T" < "$I"`) {
		t.Fatalf("缺默认 runner: %s", s)
	}
	if !strings.Contains(s, `rm -f "$T" "$B" "$I"`) {
		t.Fatalf("缺清理逻辑: %s", s)
	}
	if strings.Contains(s, "sudo") {
		t.Fatalf("无 become 时不应出现 sudo: %s", s)
	}
	if strings.Contains(s, "chmod 644") {
		t.Fatalf("不得出现 0644 降级（会把脚本内口令暴露给同机用户）: %s", s)
	}
}

func TestWrapScriptEnvAndBecome(t *testing.T) {
	req := conn.ExecRequest{
		Script:     "env | grep FOO",
		Env:        map[string]string{"FOO": "bar"},
		BecomeUser: "app",
	}
	s := WrapScript(req, "")
	// env 与脚本体一起经 stdin 投递（WrapStdin 侧）
	stdin := WrapStdin(req, "")
	body := base64.StdEncoding.EncodeToString([]byte("export FOO='bar'\nenv | grep FOO"))
	if !strings.Contains(stdin, body) {
		t.Fatalf("env 未随脚本体投递: %s", stdin)
	}
	if strings.Contains(s, body) {
		t.Fatalf("脚本体不得出现在命令行: %s", s)
	}
	if !strings.Contains(s, `sudo -n -u 'app' -- sh "$T" < "$I"`) {
		t.Fatalf("become runner 错误: %s", s)
	}
	// 属主收敛：先 chmod（属主仍是登录用户）再 sudo chown；失败即失败
	if !strings.Contains(s, `chmod 700 "$T" && sudo -n -- chown 'app' "$T"`) {
		t.Fatalf("become 权限收敛逻辑错误: %s", s)
	}
	if strings.Contains(s, "chmod 644") {
		t.Fatalf("become 不得回退 0644: %s", s)
	}
}

func TestWrapScriptSudoPasswordSameSession(t *testing.T) {
	// become + 密码：凭证预热必须发生在同一会话内（跨会话 sudo 票据不共享），
	// 密码经 stdin 首行注入、不进脚本内容
	req := conn.ExecRequest{Script: "id", BecomeUser: "root"}
	s := WrapScript(req, "s3cret")
	stdin := WrapStdin(req, "s3cret")
	if strings.Contains(s, "s3cret") || strings.Contains(stdin, "s3cret") && !strings.HasPrefix(stdin, "s3cret\n") {
		t.Fatalf("密码只应作为 stdin 首行出现: %s", s)
	}
	if !strings.Contains(s, "sudo -S -p '' -v") {
		t.Fatalf("缺少同会话凭证预热: %s", s)
	}
	if !strings.Contains(s, "read -r WDP_SUDO_PW") {
		t.Fatalf("缺少 stdin 密码读取: %s", s)
	}
	if !strings.Contains(s, "unset WDP_SUDO_PW") {
		t.Fatalf("密码变量用后必须销毁: %s", s)
	}
}

func TestWrapScriptEnvKeySanitized(t *testing.T) {
	// 非法 env 键应被忽略而不是注入脚本
	req := conn.ExecRequest{
		Script: "true",
		Env:    map[string]string{"BAD KEY": "x", "OK_1": "y"},
	}
	stdin := WrapStdin(req, "")
	if strings.Contains(stdin, base64.StdEncoding.EncodeToString([]byte("export BAD KEY"))) {
		t.Fatalf("非法键泄漏: %s", stdin)
	}
	body := base64.StdEncoding.EncodeToString([]byte("export OK_1='y'\ntrue"))
	if !strings.Contains(stdin, body) {
		t.Fatalf("合法键缺失: %s", stdin)
	}
}

// TestWrapScriptEndToEnd 真跑一遍协议（本机 sh）：脚本体经 stdin 投递、
// 哨兵行后的任务 stdin 字节精确地到达脚本、退出码透传。
func TestWrapScriptEndToEnd(t *testing.T) {
	req := conn.ExecRequest{
		Script: "echo out; read line; echo \"got:$line\"",
		Stdin:  "hello world\n",
	}
	script := WrapScript(req, "")
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdin = strings.NewReader(WrapStdin(req, ""))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("包装脚本执行失败: %v (stderr=%s)", err, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "out") {
		t.Fatalf("脚本体未执行: %q (stderr=%s)", got, stderr.String())
	}
	if !strings.Contains(got, "got:hello world") {
		t.Fatalf("任务 stdin 未字节精确送达脚本: %q", got)
	}

	// 退出码透传
	req2 := conn.ExecRequest{Script: "exit 7"}
	cmd2 := exec.Command("sh", "-c", WrapScript(req2, ""))
	cmd2.Stdin = strings.NewReader(WrapStdin(req2, ""))
	err := cmd2.Run()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 7 {
		t.Fatalf("退出码应透传 7: %v", err)
	}

	// 多行/特殊字符脚本体
	req3 := conn.ExecRequest{Script: "printf '%s\\n' 'a b' \"$(echo nested)\" '#hash' 'quote'\\''s'"}
	cmd3 := exec.Command("sh", "-c", WrapScript(req3, ""))
	cmd3.Stdin = strings.NewReader(WrapStdin(req3, ""))
	out3, err := cmd3.Output()
	if err != nil {
		t.Fatalf("特殊字符脚本失败: %v", err)
	}
	for _, want := range []string{"a b", "nested", "#hash", "quote's"} {
		if !strings.Contains(string(out3), want) {
			t.Fatalf("脚本输出缺少 %q: %q", want, out3)
		}
	}
}
