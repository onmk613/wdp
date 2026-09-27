package module

// 审查修复回归测试：package become 守卫、putFile check 模式属性漂移
// （内容变更 + 属性漂移同时发生）、copy content:"" 空文件、debug msg/var
// 类型断言失败显式报错、wait_for 轮询最小间隔、Diff 仅在 --diff 下填充。

import (
	"strings"
	"testing"
	"time"

	"wdp/internal/conn"
)

// pkgFake 构造 debian(apt) 包管理器 Fake：installed 控制包是否已安装，
// upgradable 控制 latest 探测结论；返回执行的命令记录。
func pkgFake(t *testing.T, installed, upgradable bool) (*RunContext, *[]string) {
	t.Helper()
	rc, f := newTestRC(t)
	var cmds []string
	f.ExecFn = func(req conn.ExecRequest) (conn.ExecResult, error) {
		s := req.Script
		cmds = append(cmds, s)
		switch {
		case strings.Contains(s, "ID_LIKE"):
			return conn.ExecResult{Code: 0, Stdout: "id=debian\nlike=\n"}, nil
		case strings.Contains(s, "dpkg-query"):
			if installed {
				return conn.ExecResult{Code: 0}, nil
			}
			return conn.ExecResult{Code: 1}, nil
		case strings.Contains(s, "apt-get -s install"):
			if upgradable {
				return conn.ExecResult{Code: 0, Stdout: "Reading package lists...\n1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n"}, nil
			}
			return conn.ExecResult{Code: 0, Stdout: "Reading package lists...\n0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n"}, nil
		}
		return conn.ExecResult{Code: 0}, nil
	}
	return rc, &cmds
}

func ranInstall(cmds *[]string) bool {
	for _, c := range *cmds {
		if strings.Contains(c, "apt-get install") {
			return true
		}
	}
	return false
}

// TestPackageBecomeGuard install/remove/upgrade 系统级写操作必须 become；
// 只读探测（check 模式、已收敛幂等判定）不要求 become。
func TestPackageBecomeGuard(t *testing.T) {
	mod := &PackageModule{}

	// install 未 become：显式报错，不执行 apt-get
	rc, cmds := pkgFake(t, false, false)
	r := mod.Run(rc, map[string]any{"name": "curl"}, "")
	if !r.Failed || !strings.Contains(r.Msg, "installing package curl requires become: true") {
		t.Fatalf("未 become 的 install 应显式失败: %+v", r)
	}
	if ranInstall(cmds) {
		t.Fatal("未 become 不应执行 apt-get install")
	}

	// install 已 become：正常执行
	rc, _ = pkgFake(t, false, false)
	rc.Become = true
	r = mod.Run(rc, map[string]any{"name": "curl"}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("become 后 install 应成功: %+v", r)
	}

	// remove 未 become
	rc, _ = pkgFake(t, true, false)
	r = mod.Run(rc, map[string]any{"name": "curl", "state": "absent"}, "")
	if !r.Failed || !strings.Contains(r.Msg, "removing package curl requires become: true") {
		t.Fatalf("未 become 的 remove 应显式失败: %+v", r)
	}

	// upgrade 未 become（latest + 有可用升级）
	rc, _ = pkgFake(t, true, true)
	r = mod.Run(rc, map[string]any{"name": "curl", "state": "latest"}, "")
	if !r.Failed || !strings.Contains(r.Msg, "upgrading package curl requires become: true") {
		t.Fatalf("未 become 的 upgrade 应显式失败: %+v", r)
	}

	// 已收敛幂等判定不需要 become：包已安装且 present → 无变更成功
	rc, cmds = pkgFake(t, true, false)
	r = mod.Run(rc, map[string]any{"name": "curl"}, "")
	if r.Failed || r.Changed {
		t.Fatalf("已收敛无需 become: %+v", r)
	}
	if ranInstall(cmds) {
		t.Fatal("已收敛不应执行 apt-get")
	}

	// check 模式只做只读探测，不需要 become
	rc, _ = pkgFake(t, false, true)
	rc.CheckMode = true
	r = mod.Run(rc, map[string]any{"name": "curl", "state": "latest"}, "")
	if r.Failed {
		t.Fatalf("check 预估不应要求 become: %+v", r)
	}
	if !r.Changed {
		t.Fatal("check 应预估 install（未安装）")
	}
}

// TestPutFileCheckAttrWithContentChange 回归：内容变更 + mode/属主同时
// 漂移时，check 预估必须同时报内容与属性（实跑会以带 mode 的上传 + chown
// 落盘），--diff 两类变化都展示。
func TestPutFileCheckAttrWithContentChange(t *testing.T) {
	rc, f := newTestRC(t)
	f.Files["/etc/app.conf"] = []byte("old\n")
	f.Modes["/etc/app.conf"] = 0o600
	rc.Become = true
	rc.CheckMode = true
	rc.DiffMode = true
	owner := "root root"
	orig := f.ExecFn
	f.ExecFn = func(req conn.ExecRequest) (conn.ExecResult, error) {
		if strings.Contains(req.Script, "%U %G") {
			return conn.ExecResult{Code: 0, Stdout: owner + "\n"}, nil
		}
		// 其余（sha256sum / stat %a）沿用 newTestRC 的通用模拟
		return orig(req)
	}

	changed, res := putFile(rc, putFileOpts{data: []byte("new\n"), dest: "/etc/app.conf", mode: modePtr(0o644), owner: "app", group: "app"})
	if res != nil && res.Failed {
		t.Fatal(res.Msg)
	}
	if !changed || !res.Changed {
		t.Fatal("内容 + 属性双漂移应预估 changed")
	}
	if !strings.Contains(res.Msg, "will be written") || !strings.Contains(res.Msg, "attributes will be corrected") {
		t.Fatalf("消息应同时提及写入与属性校正: %q", res.Msg)
	}
	for _, want := range []string{"-old", "+new", "- mode: 0600", "+ mode: 0644", "- owner: root:root", "+ owner: app:app"} {
		if !strings.Contains(res.Diff, want) {
			t.Fatalf("diff 应包含 %q:\n%s", want, res.Diff)
		}
	}

	// 对照：内容与属性全部一致 → 不变更
	rc2, f2 := newTestRC(t)
	f2.Files["/etc/app.conf"] = []byte("new\n")
	f2.Modes["/etc/app.conf"] = 0o644
	rc2.Become = true
	rc2.CheckMode = true
	rc2.DiffMode = true
	orig2 := f2.ExecFn
	f2.ExecFn = func(req conn.ExecRequest) (conn.ExecResult, error) {
		if strings.Contains(req.Script, "%U %G") {
			return conn.ExecResult{Code: 0, Stdout: "app app\n"}, nil
		}
		return orig2(req)
	}
	changed2, res2 := putFile(rc2, putFileOpts{data: []byte("new\n"), dest: "/etc/app.conf", mode: modePtr(0o644), owner: "app", group: "app"})
	if res2 != nil && res2.Failed {
		t.Fatal(res2.Msg)
	}
	if changed2 || res2.Changed {
		t.Fatalf("全一致不应预估变更: %+v", res2)
	}
}

// TestCopyEmptyContent 回归：content: ""（显式空串）应写空文件而非报
// "缺 content 或 src"；与 src 同时给（哪怕空串）仍报互斥。
func TestCopyEmptyContent(t *testing.T) {
	rc, f := newTestRC(t)
	mod := &CopyModule{}

	r := mod.Run(rc, map[string]any{"dest": "/tmp/empty.conf", "content": ""}, "")
	if r.Failed {
		t.Fatalf("content: \"\" 应写空文件: %s", r.Msg)
	}
	if !r.Changed {
		t.Fatal("远端无文件应 changed")
	}
	if got, ok := f.File("/tmp/empty.conf"); !ok || got != "" {
		t.Fatalf("远端应是空文件: %q ok=%v", got, ok)
	}

	// 幂等：再次执行空内容不再变更
	r = mod.Run(rc, map[string]any{"dest": "/tmp/empty.conf", "content": ""}, "")
	if r.Failed || r.Changed {
		t.Fatalf("空文件幂等: %+v", r)
	}

	// 两个键都不给：报错
	if r := mod.Run(rc, map[string]any{"dest": "/tmp/x"}, ""); !r.Failed || !strings.Contains(r.Msg, "content or src") {
		t.Fatalf("缺参应报错: %+v", r)
	}
	// content（空串）+ src 同时给：互斥报错
	if r := mod.Run(rc, map[string]any{"dest": "/tmp/x", "content": "", "src": "f.txt"}, ""); !r.Failed || !strings.Contains(r.Msg, "not both") {
		t.Fatalf("互斥应报错: %+v", r)
	}
	// src 为空串：显式报错而非当作未提供
	if r := mod.Run(rc, map[string]any{"dest": "/tmp/x", "src": ""}, ""); !r.Failed || !strings.Contains(r.Msg, "src must not be empty") {
		t.Fatalf("空 src 应报错: %+v", r)
	}
}

// TestDebugNonStringMsgAndVar 回归：msg/var 显式给了但类型不是字符串时
// 显式报类型错误，而非静默跳过后误报 "requires var or msg"。
func TestDebugNonStringMsgAndVar(t *testing.T) {
	rc, _ := newTestRC(t)
	mod := &DebugModule{}

	r := mod.Run(rc, map[string]any{"msg": 42}, "")
	if !r.Failed || !strings.Contains(r.Msg, "msg must be a string") {
		t.Fatalf("非字符串 msg 应报类型错误: %+v", r)
	}
	r = mod.Run(rc, map[string]any{"var": []string{"a"}}, "")
	if !r.Failed || !strings.Contains(r.Msg, "var must be a string") {
		t.Fatalf("非字符串 var 应报类型错误: %+v", r)
	}
	// 正常路径不受影响
	r = mod.Run(rc, map[string]any{"msg": "hello"}, "")
	if r.Failed || r.Msg != "hello" {
		t.Fatalf("字符串 msg 应正常输出: %+v", r)
	}
	// 什么都没给：维持原有报错
	if r := mod.Run(rc, map[string]any{}, ""); !r.Failed || !strings.Contains(r.Msg, "requires var or msg") {
		t.Fatalf("无参应报 requires var or msg: %+v", r)
	}
}

// TestWaitForMinPollInterval 回归：sleep: 0 合法但轮询必须有最小间隔——
// path 探测每轮一次远端 exec，无间隔即高频轰炸连接层。1s 窗口内探测
// 次数必须是"个位数"（旧实现是忙等轰炸）。
func TestWaitForMinPollInterval(t *testing.T) {
	rc, f := newTestRC(t)
	probes := 0
	orig := f.ExecFn
	f.ExecFn = func(req conn.ExecRequest) (conn.ExecResult, error) {
		if strings.Contains(req.Script, `[ -L "$p" ]`) {
			probes++
		}
		return orig(req)
	}
	mod := &WaitForModule{}
	start := time.Now()
	r := mod.Run(rc, map[string]any{
		"path": "/tmp/never-exists", "timeout": 1, "sleep": 0,
	}, "")
	elapsed := time.Since(start)
	if !r.Failed {
		t.Fatalf("条件不可满足应超时失败: %+v", r)
	}
	if probes > 3 {
		t.Fatalf("1s 窗口探测 %d 次，轮询未设最小间隔（防连接层轰炸）", probes)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("总耗时 %v，应等待满约 1s 的 timeout 窗口", elapsed)
	}
}

// TestDiffOnlyInDiffMode 回归：Result.Diff 契约是"--diff 模式：内容级差异"
// （executor 无条件透传），file/package/systemd_unit 不得在未开 --diff 时
// 填充 Diff；file 的 absent 预估在 --diff 下与 user/group 同口径输出
// "- <path> (will be deleted)"。
func TestDiffOnlyInDiffMode(t *testing.T) {
	// file：实跑变更（touch 新建）未开 --diff 不带 Diff
	rc, _ := newTestRC(t)
	r := (&FileModule{}).Run(rc, map[string]any{"path": "/data/f", "state": "touch"}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("touch 实跑: %+v", r)
	}
	if r.Diff != "" {
		t.Fatalf("未开 --diff 实跑不应带 diff: %q", r.Diff)
	}

	// file：check 预估（mode 漂移）未开 --diff 不带 Diff，开了才带 before→after
	rc, f := newTestRC(t)
	f.Files["/etc/app.conf"] = []byte("x")
	f.Modes["/etc/app.conf"] = 0o600
	rc.CheckMode = true
	r = (&FileModule{}).Run(rc, map[string]any{"path": "/etc/app.conf", "mode": "0644"}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("mode 漂移 check 预估: %+v", r)
	}
	if r.Diff != "" {
		t.Fatalf("未开 --diff 的 check 不应带 diff: %q", r.Diff)
	}
	rc.DiffMode = true
	r = (&FileModule{}).Run(rc, map[string]any{"path": "/etc/app.conf", "mode": "0644"}, "")
	if !strings.Contains(r.Diff, "- mode: 0600") || !strings.Contains(r.Diff, "+ mode: 0644") {
		t.Fatalf("--diff 下的 check 应带 mode before→after: %q", r.Diff)
	}

	// file：check + absent 在 --diff 下输出删除行（对齐 user/group）
	rc, f = newTestRC(t)
	f.Files["/old/app"] = []byte("x")
	rc.CheckMode = true
	rc.DiffMode = true
	r = (&FileModule{}).Run(rc, map[string]any{"path": "/old/app", "state": "absent"}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("check+absent 预估: %+v", r)
	}
	if r.Diff != "- /old/app (will be deleted)" {
		t.Fatalf("absent diff: %q", r.Diff)
	}

	// package：check 预估未开 --diff 不带 Diff，开了才带逐包增删
	rc, _ = pkgFake(t, false, false)
	rc.CheckMode = true
	r = (&PackageModule{}).Run(rc, map[string]any{"name": "curl"}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("package check 预估: %+v", r)
	}
	if r.Diff != "" {
		t.Fatalf("未开 --diff 的 package check 不应带 diff: %q", r.Diff)
	}
	rc, _ = pkgFake(t, false, false)
	rc.CheckMode = true
	rc.DiffMode = true
	r = (&PackageModule{}).Run(rc, map[string]any{"name": "curl"}, "")
	if !strings.Contains(r.Diff, "+ curl") {
		t.Fatalf("--diff 下的 package check 应带 diff: %q", r.Diff)
	}

	// systemd_unit：check 预估（服务将启动/自启）未开 --diff 不带 Diff
	rc, _, _ = newUnitRC(t)
	rc.CheckMode = true
	r = (&SystemdUnitModule{}).Run(rc, map[string]any{
		"name": "myapp.service", "state": "started", "enabled": true,
	}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("systemd_unit check 预估: %+v", r)
	}
	if r.Diff != "" {
		t.Fatalf("未开 --diff 的 systemd_unit check 不应带 diff: %q", r.Diff)
	}
}
