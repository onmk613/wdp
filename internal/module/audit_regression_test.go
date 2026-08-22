package module

// 审计修复回归：putFile 属主漂移探测（check 预估 + 实跑 changed）、
// zypper latest 探测（表解析 + 错误传播）。

import (
	"strings"
	"testing"

	"wdp/internal/connection"
)

// ownerFake 构造带属主探测的 Fake：内容校验和与 /etc/app.conf 固定内容一致。
func ownerFake(t *testing.T, current string) (*RunContext, *connection.Fake) {
	t.Helper()
	rc, f := newTestRC(t)
	data := []byte("k=v\n")
	sum := sha256hex(data)
	f.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
		s := req.Script
		switch {
		case strings.Contains(s, "sha256sum"):
			return connection.ExecResult{Code: 0, Stdout: sum + "  /etc/app.conf\n"}, nil
		case strings.Contains(s, "stat -c '%U %G'"), strings.Contains(s, "stat -f '%Su %Sg'"):
			return connection.ExecResult{Code: 0, Stdout: current + "\n"}, nil
		case strings.Contains(s, "stat -c %a"), strings.Contains(s, "stat -f %Lp"):
			return connection.ExecResult{Code: 0, Stdout: "644"}, nil
		case strings.Contains(s, "chown"):
			current = "app app"
			return connection.ExecResult{Code: 0}, nil
		}
		return connection.ExecResult{Code: 0}, nil
	}
	return rc, f
}

// TestPutFileOwnerDrift 回归：内容与权限一致但属主漂移时——
// check 模式预估 changed（此前漏报）；实跑校正并计入 changed（此前修复不报）。
func TestPutFileOwnerDrift(t *testing.T) {
	// check 模式：漂移 → 预估 changed
	rc, _ := ownerFake(t, "root root")
	rc.Become = true
	rc.CheckMode = true
	changed, res := putFile(rc, []byte("k=v\n"), "/etc/app.conf", 0o644, false, true, "app", "app")
	if res != nil && res.Failed {
		t.Fatal(res.Msg)
	}
	if !changed {
		t.Fatal("check 模式属主漂移应预估 changed（此前漏报，notify 不触发）")
	}

	// 实跑：校正属主并报 changed
	rc2, _ := ownerFake(t, "root root")
	rc2.Become = true
	changed2, res2 := putFile(rc2, []byte("k=v\n"), "/etc/app.conf", 0o644, false, true, "app", "app")
	if res2 != nil && res2.Failed {
		t.Fatal(res2.Msg)
	}
	if !changed2 {
		t.Fatal("实跑修复属主应报 changed")
	}

	// 已一致：不报 changed、不执行 chown
	rc3, f3 := ownerFake(t, "app app")
	rc3.Become = true
	chowned := false
	orig := f3.ExecFn
	f3.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
		if strings.Contains(req.Script, "chown") {
			chowned = true
		}
		return orig(req)
	}
	changed3, res3 := putFile(rc3, []byte("k=v\n"), "/etc/app.conf", 0o644, false, true, "app", "app")
	if res3 != nil && res3.Failed {
		t.Fatal(res3.Msg)
	}
	if changed3 || chowned {
		t.Fatalf("属主一致时不应报 changed/执行 chown: changed=%v chowned=%v", changed3, chowned)
	}
}

// TestZypperUpgradableProbe 回归：zypper list-updates 的位置参数是仓库名，
// 旧实现把包名传进去恒报"无升级"；现全量列表 + Name 列精确匹配，
// 且 zypper 自身错误（rc!=0）显式失败而非假幂等。
func TestZypperUpgradableProbe(t *testing.T) {
	rc, f := newTestRC(t)
	table := "S | Repository | Name      | Current Version | Available Version | Arch\n" +
		"--+------------+-----------+-----------------+-------------------+-------\n" +
		"v | repo-main  | nginx     | 1.24.0          | 1.26.0            | x86_64\n" +
		"v | repo-main  | postgres  | 15.2            | 15.4              | x86_64\n"
	fail := false
	f.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
		if strings.Contains(req.Script, "zypper") {
			if fail {
				return connection.ExecResult{Code: 6, Stderr: "Repository 'repo-main' is invalid"}, nil
			}
			return connection.ExecResult{Code: 0, Stdout: table}, nil
		}
		return connection.ExecResult{Code: 0}, nil
	}
	p := &pkgManager{kind: "zypper", family: "suse"}

	up, bad := p.upgradable(rc, "nginx")
	if bad != nil {
		t.Fatal(bad.Msg)
	}
	if !up {
		t.Fatal("表中有 nginx 更新行应判有升级")
	}
	up, bad = p.upgradable(rc, "nothere")
	if bad != nil {
		t.Fatal(bad.Msg)
	}
	if up {
		t.Fatal("表中无 nothere 行应判无升级")
	}
	fail = true
	up, bad = p.upgradable(rc, "nginx")
	if bad == nil || up {
		t.Fatalf("zypper 错误应显式失败而非判无升级: up=%v bad=%v", up, bad)
	}
}

// TestArgModeRejectsSpecialBits 回归：mode 4755/2755 会被全链路 Perm() 静默
// 丢弃成错误权限（4755 → 0313），现显式拒绝；带引号 "0999" 同样拒绝。
func TestArgModeRejectsSpecialBits(t *testing.T) {
	if _, ok := argMode(map[string]any{"mode": "4755"}, "mode"); ok {
		t.Fatal(`mode "4755"（setuid）应被拒绝`)
	}
	if _, ok := argMode(map[string]any{"mode": 4755}, "mode"); ok {
		t.Fatal("mode 4755（yaml 十进制整数）应被拒绝")
	}
	if _, ok := argMode(map[string]any{"mode": "0999"}, "mode"); ok {
		t.Fatal(`mode "0999"（八进制非法数字）应被拒绝`)
	}
	if m, ok := argMode(map[string]any{"mode": "0755"}, "mode"); !ok || m.Perm() != 0o755 {
		t.Fatal(`mode "0755" 应正常解析为 0755`)
	}
}
