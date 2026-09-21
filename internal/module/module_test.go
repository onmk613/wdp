package module

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	"wdp/internal/conn"
	"wdp/internal/conn/fake"
	"wdp/internal/model"
)

// newTestRC 构造带 Fake 连接的模块执行上下文。
// Fake 的 Exec 模拟了远端 sha256sum / stat / 探测 / mkdir / touch / rm 行为。
func newTestRC(t *testing.T) (*RunContext, *fake.Fake) {
	t.Helper()
	rc, f, _ := newTestRCState(t)
	return rc, f
}

// newTestRCState 同 newTestRC，但额外返回远端目录状态表（mkdir -p 的记录）：
// "某路径是否被真实创建"类断言（如 check 模式不得建目录）需要它。
func newTestRCState(t *testing.T) (*RunContext, *fake.Fake, map[string]bool) {
	t.Helper()
	fake := fake.NewFake(&model.Host{Name: "test"})
	dirs := map[string]bool{}
	fake.ExecFn = func(req conn.ExecRequest) (conn.ExecResult, error) {
		s := req.Script
		switch {
		case strings.Contains(s, `[ -f "$p" ] || exit 4`) && !strings.Contains(s, "sha256sum"): // 存在性探测（3=不存在 4=非普通文件）
			path := extractQuoted(s, "p=")
			switch {
			case dirs[path]:
				return conn.ExecResult{Code: 4}, nil
			case fake.Files[path] == nil:
				return conn.ExecResult{Code: 3}, nil
			}
			return conn.ExecResult{Code: 0}, nil
		case strings.Contains(s, "sha256sum"):
			path := extractQuoted(s, "p=")
			if dirs[path] {
				return conn.ExecResult{Code: 4}, nil // 既有目录：非普通文件
			}
			data, ok := fake.Files[path]
			if !ok {
				return conn.ExecResult{Code: 3}, nil
			}
			h := sha256.Sum256(data)
			return conn.ExecResult{Code: 0, Stdout: hex.EncodeToString(h[:]) + "  " + path + "\n"}, nil
		case strings.Contains(s, "stat -c"):
			path := extractQuoted(s, "p=")
			mode, ok := fake.Modes[path]
			if !ok {
				return conn.ExecResult{Code: 3}, nil
			}
			return conn.ExecResult{Code: 0, Stdout: fmt.Sprintf("%o", mode.Perm())}, nil
		case strings.Contains(s, "mkdir -p"):
			path := firstQuoted(s)
			dirs[path] = true
			return conn.ExecResult{}, nil
		case strings.Contains(s, "touch --"):
			path := firstQuoted(s)
			fake.Files[path] = []byte{}
			fake.Modes[path] = 0o644
			return conn.ExecResult{}, nil
		case strings.Contains(s, "rm -rf"):
			path := firstQuoted(s)
			delete(fake.Files, path)
			delete(fake.Modes, path)
			delete(dirs, path)
			return conn.ExecResult{}, nil
		case strings.Contains(s, "[ -L"):
			path := extractQuoted(s, "p=")
			switch {
			case dirs[path]:
				return conn.ExecResult{Code: 0, Stdout: "directory\n"}, nil
			default:
				_, ok := fake.Files[path]
				kind := "missing"
				if ok {
					kind = "file"
				}
				return conn.ExecResult{Code: 0, Stdout: kind + "\n"}, nil
			}
		default:
			return conn.ExecResult{}, nil
		}
	}
	rc := &RunContext{
		Ctx:     context.Background(),
		Conn:    fake,
		Host:    fake.Host,
		Vars:    map[string]any{},
		BaseDir: ".",
	}
	return rc, fake, dirs
}

// extractQuoted 从脚本中提取 p='路径' 形式的值。
func extractQuoted(script, prefix string) string {
	_, after, ok := strings.Cut(script, prefix)
	if !ok {
		return ""
	}
	rest := after
	if !strings.HasPrefix(rest, "'") {
		return ""
	}
	end := strings.Index(rest[1:], "'")
	if end < 0 {
		return ""
	}
	return rest[1 : 1+end]
}

// firstQuoted 提取脚本中第一个单引号字符串。
func firstQuoted(script string) string {
	_, after, ok := strings.Cut(script, "'")
	if !ok {
		return ""
	}
	rest := after
	before0, _, ok0 := strings.Cut(rest, "'")
	if !ok0 {
		return ""
	}
	return before0
}

func TestCopyModuleIdempotent(t *testing.T) {
	rc, fake := newTestRC(t)
	mod := &CopyModule{}

	r1 := mod.Run(rc, map[string]any{"content": "hello", "dest": "/etc/motd"}, "")
	if r1.Failed {
		t.Fatalf("第一次 copy 失败: %s", r1.Msg)
	}
	if !r1.Changed {
		t.Fatal("第一次 copy 应 changed")
	}
	if got, _ := fake.File("/etc/motd"); got != "hello" {
		t.Fatalf("内容 %q", got)
	}

	r2 := mod.Run(rc, map[string]any{"content": "hello", "dest": "/etc/motd"}, "")
	if r2.Failed {
		t.Fatalf("第二次 copy 失败: %s", r2.Msg)
	}
	if r2.Changed {
		t.Fatal("内容一致时不应 changed")
	}

	r3 := mod.Run(rc, map[string]any{"content": "world", "dest": "/etc/motd"}, "")
	if r3.Failed || !r3.Changed {
		t.Fatalf("内容变化应 changed: %+v", r3)
	}
}

func TestCopyModuleMissingDest(t *testing.T) {
	rc, _ := newTestRC(t)
	mod := &CopyModule{}
	r := mod.Run(rc, map[string]any{"dest": "/x"}, "")
	if !r.Failed {
		t.Fatal("缺 content/src 应失败")
	}
	r = mod.Run(rc, map[string]any{"content": "a", "src": "b", "dest": "/x"}, "")
	if !r.Failed {
		t.Fatal("content 与 src 同时给应失败")
	}
}

func TestTemplateModule(t *testing.T) {
	rc, fake := newTestRC(t)
	rc.Vars["name"] = "wdp"
	// 写本地模板文件
	dir := t.TempDir()
	tpl := dir + "/app.conf"
	if err := writeLocal(tpl, "app={{ .name }}\n"); err != nil {
		t.Fatal(err)
	}
	rc.BaseDir = dir

	mod := &TemplateModule{}
	r := mod.Run(rc, map[string]any{"src": "app.conf", "dest": "/opt/app.conf"}, "")
	if r.Failed {
		t.Fatalf("template 失败: %s", r.Msg)
	}
	if got, _ := fake.File("/opt/app.conf"); got != "app=wdp\n" {
		t.Fatalf("渲染结果 %q", got)
	}
}

func TestFileModuleStates(t *testing.T) {
	rc, _ := newTestRC(t)
	mod := &FileModule{}

	// 创建目录
	r := mod.Run(rc, map[string]any{"path": "/data", "state": "directory"}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("创建目录: %+v", r)
	}
	// 幂等
	r = mod.Run(rc, map[string]any{"path": "/data", "state": "directory"}, "")
	if r.Failed || r.Changed {
		t.Fatalf("目录已存在: %+v", r)
	}
	// touch 文件（Fake 不执行 touch 副作用，仅验证模块流程）
	r = mod.Run(rc, map[string]any{"path": "/data/f", "state": "touch"}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("touch: %+v", r)
	}
	// 不支持的 state
	r = mod.Run(rc, map[string]any{"path": "/x", "state": "weird"}, "")
	if !r.Failed {
		t.Fatal("非法 state 应失败")
	}
	// 缺 path
	r = mod.Run(rc, map[string]any{"state": "directory"}, "")
	if !r.Failed {
		t.Fatal("缺 path 应失败")
	}
}

func TestShellModule(t *testing.T) {
	rc, _ := newTestRC(t)
	fake := rc.Conn.(*fake.Fake)
	fake.ExecFn = func(req conn.ExecRequest) (conn.ExecResult, error) {
		return conn.ExecResult{Code: 0, Stdout: "up 1 day\n"}, nil
	}
	mod := &ShellModule{}
	r := mod.Run(rc, nil, "uptime")
	if r.Failed || r.Stdout != "up 1 day\n" || !r.Changed {
		t.Fatalf("%+v", r)
	}

	r = mod.Run(rc, nil, "")
	if !r.Failed {
		t.Fatal("空命令应失败")
	}

	fake.ExecFn = func(req conn.ExecRequest) (conn.ExecResult, error) {
		return conn.ExecResult{Code: 2, Stderr: "boom"}, nil
	}
	r = mod.Run(rc, nil, "false")
	if !r.Failed || r.Rc != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestShellCreatesSkips(t *testing.T) {
	rc, fake := newTestRC(t)
	fake.ExecFn = func(req conn.ExecRequest) (conn.ExecResult, error) {
		if strings.Contains(req.Script, "[ -e") {
			return conn.ExecResult{Code: 0}, nil // 文件存在
		}
		return conn.ExecResult{Code: 0}, nil
	}
	mod := &ShellModule{}
	r := mod.Run(rc, map[string]any{"creates": "/done"}, "heavy-job")
	if r.Failed || r.Changed {
		t.Fatalf("creates 已存在应跳过: %+v", r)
	}
}

func writeLocal(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
