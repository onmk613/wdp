package sshconn

import (
	"encoding/base64"
	"strings"
	"testing"

	"wdp/internal/connection"
)

func TestWrapScriptPlain(t *testing.T) {
	s := WrapScript(connection.ExecRequest{Script: "echo hi"}, "")
	// 脚本体应经 base64 传输
	if !strings.Contains(s, base64.StdEncoding.EncodeToString([]byte("echo hi"))) {
		t.Fatalf("缺少 base64 脚本体: %s", s)
	}
	if !strings.Contains(s, `sh "$T"`) {
		t.Fatalf("缺默认 runner: %s", s)
	}
	if !strings.Contains(s, "rm -f \"$T\"") {
		t.Fatalf("缺清理逻辑: %s", s)
	}
	if strings.Contains(s, "sudo") {
		t.Fatalf("无 become 时不应出现 sudo: %s", s)
	}
}

func TestWrapScriptEnvAndBecome(t *testing.T) {
	s := WrapScript(connection.ExecRequest{
		Script:     "env | grep FOO",
		Env:        map[string]string{"FOO": "bar"},
		BecomeUser: "app",
	}, "")
	body := base64.StdEncoding.EncodeToString([]byte("export FOO='bar'\nenv | grep FOO"))
	if !strings.Contains(s, body) {
		t.Fatalf("env 未导入脚本体: %s", s)
	}
	if !strings.Contains(s, `sudo -n -u 'app' -- sh "$T"`) {
		t.Fatalf("become runner 错误: %s", s)
	}
	// 属主收敛优先：chown 到目标用户后保持 0700，失败才回退 0644
	if !strings.Contains(s, "chown 'app' \"$T\" 2>/dev/null && chmod 700 \"$T\" || chmod 644 \"$T\"") {
		t.Fatalf("become 权限收敛逻辑错误: %s", s)
	}
}

func TestWrapScriptSudoPasswordSameSession(t *testing.T) {
	// become + 密码：凭证预热必须发生在同一会话内（跨会话 sudo 票据不共享），
	// 密码经 stdin 首行注入、不进脚本内容
	s := WrapScript(connection.ExecRequest{Script: "id", BecomeUser: "root"}, "s3cret")
	if strings.Contains(s, "s3cret") {
		t.Fatalf("密码不得出现在脚本内容中: %s", s)
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
	s := WrapScript(connection.ExecRequest{
		Script: "true",
		Env:    map[string]string{"BAD KEY": "x", "OK_1": "y"},
	}, "")
	if strings.Contains(s, "BAD KEY") {
		t.Fatalf("非法键泄漏: %s", s)
	}
	body := base64.StdEncoding.EncodeToString([]byte("export OK_1='y'\ntrue"))
	if !strings.Contains(s, body) {
		t.Fatalf("合法键缺失: %s", s)
	}
}
