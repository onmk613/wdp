package module

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/connection"
)

// TestScriptArgsQuoted 回归：free-form 参数曾原文拼进 sh -c，元字符会被
// 远端 shell 解释。现在必须逐词 Quote 后拼接，参数字面传给脚本。
func TestScriptArgsQuoted(t *testing.T) {
	rc, fake := newTestRC(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "migrate.sh")
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var sent string
	oldExecFn := fake.ExecFn
	fake.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
		if strings.Contains(req.Script, ".wdp-script-") && !strings.HasPrefix(strings.TrimSpace(req.Script), "rm ") {
			sent = req.Script // 捕获真正执行脚本的命令
		}
		return oldExecFn(req)
	}

	res := (&ScriptModule{}).Run(rc, map[string]any{"src": src}, `--tag "a b" $(pwned) ;reboot`)
	if res.Failed {
		t.Fatalf("执行失败: %s", res.Msg)
	}
	for _, token := range []string{`'--tag'`, `'a b'`, `'$(pwned)'`, `';reboot'`} {
		if !strings.Contains(sent, token) {
			t.Errorf("脚本命令 %q 缺少转义词 %s", sent, token)
		}
	}
	if strings.Contains(sent, "$(pwned) ;") { // 原文未转义即被 shell 解释
		t.Errorf("脚本命令含未转义元字符: %q", sent)
	}
}

// TestScriptArgsUnterminated 引号未闭合的 free-form 参数应 fail-loud。
func TestScriptArgsUnterminated(t *testing.T) {
	rc, _ := newTestRC(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "migrate.sh")
	if err := os.WriteFile(src, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := (&ScriptModule{}).Run(rc, map[string]any{"src": src}, `--msg 'unclosed`)
	if !res.Failed {
		t.Fatal("引号未闭合的参数应报错")
	}
}
