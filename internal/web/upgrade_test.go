package web

// agent 远程升级：同版本幂等短路（真实 mTLS loopback agent）、离线拒绝、
// 替换脚本形态（/proc/$PPID/exe 定位 + 原子 mv + systemd 分支）。
// force 全流程会真实改写目标机文件，不在单测执行——由冒烟环境人工验证。

import (
	"net/http"
	"strings"
	"testing"

	"wdp/internal/agent"
	"wdp/internal/buildinfo"
	"wdp/internal/store"
)

func TestUpgradeAlreadyLatest(t *testing.T) {
	s, _ := newAppServerMTLS(t, 18764)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	// 注入定版构建信息：测试二进制默认 Commit=="none"（未注入 ldflags），
	// 属「未定版本」形态——版本串不反映内容，幂等短路被有意跳过（见
	// upgradeAgent）。注入后 loopback agent（同进程，实时取值）与 server
	// 报同一定版串，短路生效
	oldCommit := buildinfo.Commit
	buildinfo.Commit = "c0ffee0"
	defer func() { buildinfo.Commit = oldCommit }()

	// loopback agent 与 server 同二进制：非 force 应幂等短路
	rec := do(t, h, "POST", "/api/hosts/1/upgrade", map[string]any{}, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("同版本应 200: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "已是最新") || !strings.Contains(rec.Body.String(), buildinfo.BuildVersion()) {
		t.Fatalf("应报告已是最新: %s", rec.Body)
	}
	if agent.BuildVersion() != buildinfo.BuildVersion() {
		t.Fatal("agent 与 server 版本口径应一致")
	}
}

func TestUpgradeOffline(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)
	if _, err := st.CreateHost(&store.Host{Name: "up-off", Address: "127.0.0.1", AgentPort: 1}); err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, "POST", "/api/hosts/1/upgrade", map[string]any{}, &token)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "不可达") {
		t.Fatalf("离线应 502: %d %s", rec.Code, rec.Body)
	}
	// 空参数校验
	if rec := do(t, h, "POST", "/api/hosts/upgrade", map[string]any{"ids": []int64{}}, &token); rec.Code != http.StatusBadRequest {
		t.Fatalf("空 ids 应 400: %d", rec.Code)
	}
}

func TestUpgradeScript(t *testing.T) {
	sc := upgradeScript("/usr/local/bin/.wdp-upgrade-abc123", "")
	for _, want := range []string{
		`readlink /proc/$PPID/exe`,                          // 旧 agent：探测二进制
		`mv -f '/usr/local/bin/.wdp-upgrade-abc123' "$DST"`, // 原子替换（单引号字面量）
		`systemctl restart wdp-agent`,                       // systemd 重启
		`WDP_NO_SYSTEMD`,                                    // 非 systemd 分支
	} {
		if !strings.Contains(sc, want) {
			t.Fatalf("升级脚本缺 %q:\n%s", want, sc)
		}
	}
}

// TestUpgradeScriptKnownPath 已知 bin_path：直接使用，不做探测。
func TestUpgradeScriptKnownPath(t *testing.T) {
	sc := upgradeScript("/tmp/x/.wdp-upgrade-1", "/tmp/x/wdp")
	if strings.Contains(sc, "readlink") || !strings.Contains(sc, `DST='/tmp/x/wdp'`) {
		t.Fatalf("已知路径应直用:\n%s", sc)
	}
}

// TestUpgradeScriptUnsafeDst dst 来自 agent 上报（不可信）：含引号/命令
// 替换语法时必须是单引号字面量，不得闭合注入。
func TestUpgradeScriptUnsafeDst(t *testing.T) {
	sc := upgradeScript("/opt/$(.wdp-upgrade-1", `/opt/x';reboot;'/wdp`)
	if !strings.Contains(sc, `DST='/opt/x'\'';reboot;'\''/wdp'`) {
		t.Fatalf("不可信 dst 应安全引用:\n%s", sc)
	}
	// tmp 与 dst 同目录：路径里的命令替换语法同样只作字面量
	if !strings.Contains(sc, `mv -f '/opt/$(.wdp-upgrade-1' "$DST"`) {
		t.Fatalf("tmp 路径应安全引用:\n%s", sc)
	}
}
