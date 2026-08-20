package module

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/connection"
)

// unitShell 模拟 systemctl + putFile 用到的 sha256sum/stat 探测。
// 文件内容落在 Fake 内存表（上传走真实 UploadFile）。
type unitShell struct {
	fake    *connection.Fake
	active  map[string]bool
	enabled map[string]bool
	runs    []string // systemctl 变更命令记录
}

func newUnitRC(t *testing.T) (*RunContext, *connection.Fake, *unitShell) {
	t.Helper()
	rc, fake := newTestRC(t)
	sh := &unitShell{
		fake:    fake,
		active:  map[string]bool{},
		enabled: map[string]bool{},
	}
	fake.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
		s := req.Script
		switch {
		case strings.Contains(s, "command -v systemctl"):
			return connection.ExecResult{Code: 0}, nil
		case strings.Contains(s, "sha256sum"):
			path := extractQuoted(s, "p=")
			data, ok := fake.Files[path]
			if !ok {
				return connection.ExecResult{Code: 3}, nil
			}
			h := sha256.Sum256(data)
			return connection.ExecResult{Code: 0, Stdout: hex.EncodeToString(h[:]) + "  " + path + "\n"}, nil
		case strings.Contains(s, "stat -c"):
			path := extractQuoted(s, "p=")
			mode, ok := fake.Modes[path]
			if !ok {
				return connection.ExecResult{Code: 3}, nil
			}
			return connection.ExecResult{Code: 0, Stdout: fmt.Sprintf("%o", mode.Perm())}, nil
		case strings.Contains(s, "systemctl is-active"):
			if sh.active[firstQuoted(s)] {
				return connection.ExecResult{Code: 0}, nil
			}
			return connection.ExecResult{Code: 3}, nil
		case strings.Contains(s, "systemctl is-enabled"):
			if sh.enabled[firstQuoted(s)] {
				return connection.ExecResult{Code: 0, Stdout: "enabled\n"}, nil
			}
			return connection.ExecResult{Code: 1, Stderr: "disabled"}, nil
		case strings.HasPrefix(s, "systemctl"):
			// 变更命令：daemon-reload / start / stop / restart / reload / enable / disable
			verb := strings.Fields(s)[1]
			if u := firstQuoted(s); u != "" {
				switch verb {
				case "start", "restart":
					sh.active[u] = true
				case "stop":
					sh.active[u] = false
				case "reload":
					sh.active[u] = true
				case "enable":
					sh.enabled[u] = true
				case "disable":
					sh.enabled[u] = false
				}
			}
			sh.runs = append(sh.runs, s)
			return connection.ExecResult{Code: 0}, nil
		default:
			return connection.ExecResult{Code: 0}, nil
		}
	}
	return rc, fake, sh
}

const unitContent = `[Unit]
Description=MyApp

[Service]
ExecStart=/opt/myapp/bin/myapp

[Install]
WantedBy=multi-user.target
`

func TestSystemdUnitDeploy(t *testing.T) {
	rc, fake, sh := newUnitRC(t)
	mod := &SystemdUnitModule{}

	r := mod.Run(rc, map[string]any{
		"name": "myapp.service", "content": unitContent,
		"state": "started", "enabled": true,
	}, "")
	if r.Failed {
		t.Fatalf("部署失败: %s", r.Msg)
	}
	if !r.Changed {
		t.Fatal("首次部署应 changed")
	}
	if got, _ := fake.File("/etc/systemd/system/myapp.service"); got != unitContent {
		t.Fatalf("unit 内容:\n%q", got)
	}
	want := []string{
		"systemctl daemon-reload",
		"systemctl start 'myapp.service'",
		"systemctl enable 'myapp.service'",
	}
	if strings.Join(sh.runs, "|") != strings.Join(want, "|") {
		t.Fatalf("systemctl 执行:\n got %v\nwant %v", sh.runs, want)
	}

	// 幂等：内容一致 + 已 active/enabled → 无变更
	r = mod.Run(rc, map[string]any{
		"name": "myapp.service", "content": unitContent,
		"state": "started", "enabled": true,
	}, "")
	if r.Failed || r.Changed {
		t.Fatalf("幂等: %+v", r)
	}
	if len(sh.runs) != 3 {
		t.Fatalf("不应再次变更: %v", sh.runs)
	}
}

func TestSystemdUnitRestartOnChange(t *testing.T) {
	rc, fake, sh := newUnitRC(t)
	mod := &SystemdUnitModule{}

	args := func(content string) map[string]any {
		return map[string]any{"name": "worker.service", "content": content, "state": "restarted", "enabled": true}
	}
	if r := mod.Run(rc, args("v1\n"), ""); r.Failed {
		t.Fatalf("首次: %s", r.Msg)
	}
	if r := mod.Run(rc, args("v2\n"), ""); r.Failed {
		t.Fatalf("内容变更: %s", r.Msg)
	}
	// 内容变更应触发 daemon-reload + restart（已 enabled 不再 enable）
	last2 := sh.runs[len(sh.runs)-2:]
	want := []string{"systemctl daemon-reload", "systemctl restart 'worker.service'"}
	if last2[0] != want[0] || last2[1] != want[1] {
		t.Fatalf("变更执行: %v (全部 %v)", last2, sh.runs)
	}
	if _, ok := fake.File("/etc/systemd/system/worker.service"); !ok {
		t.Fatal("unit 文件未落盘")
	}

	// daemon_reload=false：内容变更不执行 daemon-reload
	before := len(sh.runs)
	r := mod.Run(rc, map[string]any{"name": "worker.service", "content": "v3\n", "state": "restarted", "daemon_reload": false}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("daemon_reload=false: %+v", r)
	}
	for _, c := range sh.runs[before:] {
		if strings.Contains(c, "daemon-reload") {
			t.Fatalf("不应 daemon-reload: %v", sh.runs[before:])
		}
	}
}

func TestSystemdUnitStoppedAndDisable(t *testing.T) {
	rc, _, sh := newUnitRC(t)
	sh.active["svc.service"] = true
	sh.enabled["svc.service"] = true
	mod := &SystemdUnitModule{}

	r := mod.Run(rc, map[string]any{
		"name": "svc.service", "content": "unit\n",
		"state": "stopped", "enabled": false,
	}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("停止+禁用: %+v", r)
	}
	last2 := sh.runs[len(sh.runs)-2:]
	if last2[0] != "systemctl stop 'svc.service'" || last2[1] != "systemctl disable 'svc.service'" {
		t.Fatalf("执行: %v", sh.runs)
	}
	if sh.active["svc.service"] || sh.enabled["svc.service"] {
		t.Fatal("模拟状态未更新")
	}
}

func TestSystemdUnitTemplateSrc(t *testing.T) {
	rc, fake, _ := newUnitRC(t)
	rc.Vars["port"] = "8080"
	dir := t.TempDir()
	tpl := filepath.Join(dir, "app.service")
	if err := os.WriteFile(tpl, []byte("ExecStart=/bin/app --port {{ .port }}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc.BaseDir = dir

	mod := &SystemdUnitModule{}
	r := mod.Run(rc, map[string]any{"name": "app.service", "src": "app.service"}, "")
	if r.Failed {
		t.Fatalf("模板部署失败: %s", r.Msg)
	}
	if got, _ := fake.File("/etc/systemd/system/app.service"); got != "ExecStart=/bin/app --port 8080\n" {
		t.Fatalf("渲染结果 %q", got)
	}
	// 无 {{ 的 src 原样分发
	if err := os.WriteFile(filepath.Join(dir, "plain.service"), []byte("plain\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r = mod.Run(rc, map[string]any{"name": "plain.service", "src": "plain.service", "dest_dir": "/etc/systemd/user"}, "")
	if r.Failed {
		t.Fatalf("原样部署失败: %s", r.Msg)
	}
	if got, _ := fake.File("/etc/systemd/user/plain.service"); got != "plain\n" {
		t.Fatalf("原样内容 %q", got)
	}
}

func TestSystemdUnitCheckMode(t *testing.T) {
	rc, fake, sh := newUnitRC(t)
	rc.CheckMode = true
	rc.DiffMode = true
	mod := &SystemdUnitModule{}

	r := mod.Run(rc, map[string]any{
		"name": "myapp.service", "content": unitContent,
		"state": "started", "enabled": true,
	}, "")
	if r.Failed {
		t.Fatalf("check 失败: %s", r.Msg)
	}
	if !r.Changed || !strings.HasPrefix(r.Msg, "[check]") {
		t.Fatalf("check 预估: %+v", r)
	}
	if !strings.Contains(r.Msg, "daemon-reload") || !strings.Contains(r.Msg, "启动") {
		t.Fatalf("check 消息: %q", r.Msg)
	}
	if !strings.Contains(r.Diff, "+ myapp.service: active") || !strings.Contains(r.Diff, "+ myapp.service: enabled") {
		t.Fatalf("check diff: %q", r.Diff)
	}
	if len(sh.runs) != 0 {
		t.Fatalf("check 不应执行 systemctl 变更: %v", sh.runs)
	}
	if len(fake.Files) != 0 {
		t.Fatal("check 不应写文件")
	}

	// 全新主机 check：文件将写入 + 服务将启动，但无任何副作用
	rc2, fake2, sh2 := newUnitRC(t)
	rc2.CheckMode = true
	mod2 := &SystemdUnitModule{}
	r2 := mod2.Run(rc2, map[string]any{"name": "myapp.service", "content": "x\n"}, "")
	if r2.Failed || !r2.Changed || !strings.Contains(r2.Msg, "unit 文件将写入") {
		t.Fatalf("check 首次应预估写入: %+v", r2)
	}
	if len(sh2.runs) != 0 || len(fake2.Files) != 0 {
		t.Fatal("check 不应有副作用")
	}
}

func TestSystemdUnitRollbackViaPutFile(t *testing.T) {
	rc, _, _ := newUnitRC(t)
	var actions []RollbackAction
	rc.Rollback = &RollbackCtx{Dir: "/var/.wdp-rollback", Record: func(a RollbackAction) { actions = append(actions, a) }}
	mod := &SystemdUnitModule{}

	// 新建 unit → 登记回滚删除
	r := mod.Run(rc, map[string]any{"name": "myapp.service", "content": "v1\n"}, "")
	if r.Failed {
		t.Fatalf("%s", r.Msg)
	}
	if len(actions) != 1 || actions[0] != (RollbackAction{Kind: "remove", Path: "/etc/systemd/system/myapp.service"}) {
		t.Fatalf("回滚登记: %+v", actions)
	}
	// 内容变更 → 快照恢复
	actions = nil
	r = mod.Run(rc, map[string]any{"name": "myapp.service", "content": "v2\n"}, "")
	if r.Failed {
		t.Fatalf("%s", r.Msg)
	}
	if len(actions) != 1 || actions[0].Kind != "restore" || actions[0].Path != "/etc/systemd/system/myapp.service" || actions[0].Shadow == "" {
		t.Fatalf("快照登记: %+v", actions)
	}
}

func TestSystemdUnitValidation(t *testing.T) {
	rc, _, _ := newUnitRC(t)
	mod := &SystemdUnitModule{}
	if r := mod.Run(rc, map[string]any{"content": "x"}, ""); !r.Failed {
		t.Fatal("缺 name 应失败")
	}
	if r := mod.Run(rc, map[string]any{"name": "a/b.service", "content": "x"}, ""); !r.Failed || !strings.Contains(r.Msg, "basename") {
		t.Fatalf("name 含 /: %+v", r)
	}
	if r := mod.Run(rc, map[string]any{"name": "a.service"}, ""); !r.Failed {
		t.Fatal("content/src 缺失应失败")
	}
	if r := mod.Run(rc, map[string]any{"name": "a.service", "content": "x", "src": "f"}, ""); !r.Failed {
		t.Fatal("content 与 src 同时给应失败")
	}
	if r := mod.Run(rc, map[string]any{"name": "a.service", "content": "x", "state": "weird"}, ""); !r.Failed || !strings.Contains(r.Msg, "state") {
		t.Fatalf("非法 state: %+v", r)
	}
}
