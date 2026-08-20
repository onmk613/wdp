package module

// 回归：state: latest 的幂等判定——无可用升级时不报 changed（check 与实跑一致）。

import (
	"strings"
	"testing"

	"wdp/internal/connection"
)

// TestAptUpgradableProbe apt 探测：模拟 "1 upgraded" 汇总行 → 有升级；
// "0 upgraded, 0 newly installed" / "already newest" → 无升级。
func TestAptUpgradableProbe(t *testing.T) {
	rc, f := newTestRC(t)
	f.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
		s := req.Script
		switch {
		case strings.Contains(s, "dpkg-query"):
			return connection.ExecResult{Code: 0}, nil // 已安装
		case strings.Contains(s, "apt-get -s"):
			if strings.Contains(s, "old-pkg") {
				// awk 遇 "0 upgraded" 行不 exit → 退出码 0
				return connection.ExecResult{Code: 0, Stdout: "0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n"}, nil
			}
			// awk 遇 "1 upgraded" 行 exit 1
			return connection.ExecResult{Code: 1, Stdout: "1 upgraded, 0 newly installed, 0 to remove and 1 not upgraded.\n"}, nil
		default:
			return connection.ExecResult{Code: 0}, nil
		}
	}
	p := &pkgManager{kind: "apt", family: "debian"}
	up, bad := p.upgradable(rc, "new-pkg")
	if bad != nil {
		t.Fatal(bad.Msg)
	}
	if !up {
		t.Fatal("1 upgraded 应判为有升级")
	}
	up, bad = p.upgradable(rc, "old-pkg")
	if bad != nil {
		t.Fatal(bad.Msg)
	}
	if up {
		t.Fatal("0 upgraded 应判为无升级")
	}
}

// TestPackageLatestIdempotent 无升级时 latest 不报 changed 且不调用 upgrade。
func TestPackageLatestIdempotent(t *testing.T) {
	rc, f := newTestRC(t)
	upgraded := false
	f.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
		s := req.Script
		switch {
		case strings.Contains(s, "os-release"):
			return connection.ExecResult{Code: 0, Stdout: "id=debian\nlike=\n"}, nil
		case strings.Contains(s, "dpkg-query"):
			return connection.ExecResult{Code: 0}, nil // 已安装
		case strings.Contains(s, "apt-get -s"):
			// 真实脚本：awk 遇 "0 upgraded" 行不 exit（退出码 0）；">0 upgraded" 才 exit 1
			return connection.ExecResult{Code: 0, Stdout: "0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n"}, nil
		case strings.Contains(s, "apt-get install"):
			upgraded = true
			return connection.ExecResult{Code: 0}, nil
		default:
			return connection.ExecResult{Code: 0}, nil
		}
	}
	m := &PackageModule{}
	res := m.Run(rc, map[string]any{"name": "nginx", "state": "latest"}, "")
	if res.Failed {
		t.Fatalf("不应失败: %s", res.Msg)
	}
	if res.Changed {
		t.Fatalf("无可用升级不应报 changed: %s", res.Msg)
	}
	if upgraded {
		t.Fatal("无可用升级不应调用 upgrade")
	}
	if !strings.Contains(res.Msg, "已是最新") {
		t.Fatalf("应说明已是最新: %s", res.Msg)
	}
	// check 模式应与实跑一致：同样"已是最新"且 Changed=false
	rc.CheckMode = true
	res = m.Run(rc, map[string]any{"name": "nginx", "state": "latest"}, "")
	if res.Changed || !strings.Contains(res.Msg, "已是最新") {
		t.Fatalf("check 模式应与实跑判定一致: %+v %s", res.Changed, res.Msg)
	}
}
