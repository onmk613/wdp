package sshconn

import (
	"encoding/base64"
	"strings"
	"testing"

	"wdp/internal/connection"
)

func TestWrapScriptPlain(t *testing.T) {
	s := WrapScript(connection.ExecRequest{Script: "echo hi"})
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
}

func TestWrapScriptEnvAndBecome(t *testing.T) {
	s := WrapScript(connection.ExecRequest{
		Script:     "env | grep FOO",
		Env:        map[string]string{"FOO": "bar"},
		BecomeUser: "app",
	})
	body := base64.StdEncoding.EncodeToString([]byte("export FOO='bar'\nenv | grep FOO"))
	if !strings.Contains(s, body) {
		t.Fatalf("env 未导入脚本体: %s", s)
	}
	if !strings.Contains(s, `sudo -n -u 'app' -- sh "$T"`) {
		t.Fatalf("become runner 错误: %s", s)
	}
	if !strings.Contains(s, "chmod 644 \"$T\"") {
		t.Fatalf("become 时应 644 便于目标用户读取: %s", s)
	}
}

func TestWrapScriptEnvKeySanitized(t *testing.T) {
	// 非法 env 键应被忽略而不是注入脚本
	s := WrapScript(connection.ExecRequest{
		Script: "true",
		Env:    map[string]string{"BAD KEY": "x", "OK_1": "y"},
	})
	if strings.Contains(s, "BAD KEY") {
		t.Fatalf("非法键泄漏: %s", s)
	}
	body := base64.StdEncoding.EncodeToString([]byte("export OK_1='y'\ntrue"))
	if !strings.Contains(s, body) {
		t.Fatalf("合法键缺失: %s", s)
	}
}
