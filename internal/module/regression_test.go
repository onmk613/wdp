package module

// 审计修复回归测试：参数校验、bool/mode 解析、fetch 路径包含、mode 漂移变更报告。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/model"
)

// TestValidateArgsRejectsTypo 未知参数报错并给出相似键建议（moed→mode）。
func TestValidateArgsRejectsTypo(t *testing.T) {
	m, _ := Get("copy")
	err := ValidateArgs(m, map[string]any{"dest": "/a", "content": "x", "moed": "0600"}, "")
	if err == nil {
		t.Fatal("拼写错误的参数应被拒绝")
	}
	if !strings.Contains(err.Error(), "moed") || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("应报出未知键与建议: %v", err)
	}
}

// TestValidateArgsRejectsBadBool 布尔参数无法解析时报错（不再静默当 false）。
func TestValidateArgsRejectsBadBool(t *testing.T) {
	m, _ := Get("copy")
	err := ValidateArgs(m, map[string]any{"dest": "/a", "content": "x", "backup": "maybe"}, "")
	if err == nil {
		t.Fatal("backup=maybe 应被拒绝")
	}
}

// TestValidateArgsRejectsBadInt int 参数无法解析时报错（不再静默当未提供，
// 否则 uid/gid/agent_port 拼错值会跳过漂移检查）；数字字面量与数字串均合法。
func TestValidateArgsRejectsBadInt(t *testing.T) {
	m, _ := Get("user")
	err := ValidateArgs(m, map[string]any{"name": "app", "uid": "1024x"}, "")
	if err == nil {
		t.Fatal("uid=1024x 应被拒绝")
	}
	if !strings.Contains(err.Error(), "uid") {
		t.Fatalf("应报出参数名 uid: %v", err)
	}
	for _, v := range []any{1024, int64(1024), "1024"} {
		if err := ValidateArgs(m, map[string]any{"name": "app", "uid": v}, ""); err != nil {
			t.Fatalf("uid=%v(%T) 应合法: %v", v, v, err)
		}
	}
	// 未提供 uid 不报错（与"提供了但非法"区分）
	if err := ValidateArgs(m, map[string]any{"name": "app"}, ""); err != nil {
		t.Fatalf("未提供 uid 应合法: %v", err)
	}
}

// TestValidateArgsAcceptsYesBool yes/on/1 是合法布尔（YAML 1.1 习惯写法）。
func TestValidateArgsAcceptsYesBool(t *testing.T) {
	m, _ := Get("copy")
	for _, v := range []any{"yes", "no", "on", "off", "1", "0", true, false} {
		if err := ValidateArgs(m, map[string]any{"dest": "/a", "content": "x", "backup": v}, ""); err != nil {
			t.Fatalf("backup=%v 应合法: %v", v, err)
		}
	}
}

// TestArgBoolCanonicalValues argBool 解析 YAML 1.1 惯用布尔。
func TestArgBoolCanonicalValues(t *testing.T) {
	for v, want := range map[any]bool{"yes": true, "YES": true, "on": true, "1": true, "no": false, "off": false, "0": false} {
		b, ok := argBool(map[string]any{"k": v}, "k")
		if !ok || b != want {
			t.Fatalf("argBool(%v) = %v,%v 期望 %v,true", v, b, ok, want)
		}
	}
	if _, ok := argBool(map[string]any{"k": "maybe"}, "k"); ok {
		t.Fatal("无法解析的值应返回 ok=false")
	}
}

// TestArgModeRejectsFloatOctal yaml.v3 把 0999 解析为 float64(999)，
// argMode 应拒绝（八进制不含 8/9），不再静默丢弃或误解析。
func TestArgModeRejectsFloatOctal(t *testing.T) {
	if _, ok := argMode(map[string]any{"mode": float64(999)}, "mode"); ok {
		t.Fatal("float64(999) 应被拒绝（八进制非法）")
	}
	// 合法八进制整数值经 float64 传入时应正常解析（yaml 解析路径）
	m, ok := argMode(map[string]any{"mode": float64(493)}, "mode")
	if !ok || m.Perm() != 0o755 {
		t.Fatalf("float64(493) 应为 0755, got %v/%o", ok, m.Perm())
	}
}

// TestFetchRejectsTraversal fetch 落盘路径逃逸 dest 时报错。
func TestFetchRejectsTraversal(t *testing.T) {
	rc, fake := newTestRC(t)
	fake.Files["/etc/passwd"] = []byte("root:x:0:0\n") // 远端文件存在
	fake.Files["/var/../../../../etc/passwd"] = []byte("root:x:0:0\n")
	m := &FetchModule{}
	// 主机名带 .. 是逃逸向量：应被 checkLocalPath 拒绝
	rc.Host.Name = "../../ESCAPED"
	res := m.Run(rc, map[string]any{
		"src":  "/etc/passwd",
		"dest": "./out",
	}, "")
	if res == nil || !res.Failed || !strings.Contains(res.Msg, "escapes") {
		t.Fatalf("应拒绝逃逸路径, got %+v", res)
	}
	// src 含 .. 同样拒绝
	rc.Host.Name = "test"
	res = m.Run(rc, map[string]any{
		"src":  "/var/../../../../etc/passwd",
		"dest": "./out",
	}, "")
	if res == nil || !res.Failed || !strings.Contains(res.Msg, "escapes") {
		t.Fatalf("src 含 .. 应被拒绝, got %+v", res)
	}
}

// TestFetchDestSandboxed fetch 的 dest 与 copy 的 src 同口径约束在 BaseDir
// 内：绝对路径与 .. 逃逸拒绝——fetch 是唯一把远端内容写回控制端的模块，
// dest 不设防即 `dest: /usr/local/bin` 可覆盖控制端任意可写文件。
func TestFetchDestSandboxed(t *testing.T) {
	rc, fake := newTestRC(t)
	rc.BaseDir = t.TempDir()
	fake.Files["/etc/passwd"] = []byte("root:x:0:0\n")
	m := &FetchModule{}
	for _, dest := range []string{"/tmp/out", "../../out"} {
		res := m.Run(rc, map[string]any{"src": "/etc/passwd", "dest": dest}, "")
		if res == nil || !res.Failed || !strings.Contains(res.Msg, "outside the chart/playbook directory") {
			t.Fatalf("dest %q 应被沙箱拒绝, got %+v", dest, res)
		}
	}
	// BaseDir 内的相对路径照常放行
	res := m.Run(rc, map[string]any{"src": "/etc/passwd", "dest": "./out"}, "")
	if res == nil || res.Failed {
		t.Fatalf("BaseDir 内相对 dest 应放行: %+v", res)
	}
}

// TestLocalReadsCapped script/template/systemd_unit 的本地读取走上限
// （回归：三处此前是裸 os.ReadFile，绕过 max_upload_mb——chart 内误放的
// 大文件在 forks 并发下是控制端 OOM 的最短路径，web 控制台进程内执行时
// 会连控制台一起带走）。
func TestLocalReadsCapped(t *testing.T) {
	rc, _ := newTestRC(t)
	rc.BaseDir = t.TempDir()
	rc.MaxUploadBytes = 64
	big := strings.Repeat("x", 200)
	for _, f := range []string{"big.sh", "big.tpl", "big.unit"} {
		if err := os.WriteFile(filepath.Join(rc.BaseDir, f), []byte(big), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name string
		run  func() *Result
	}{
		{"script", func() *Result {
			return (&ScriptModule{}).Run(rc, map[string]any{"src": "big.sh"}, "")
		}},
		{"template", func() *Result {
			return (&TemplateModule{}).Run(rc, map[string]any{"src": "big.tpl", "dest": "/tmp/x"}, "")
		}},
		{"systemd_unit", func() *Result {
			return (&SystemdUnitModule{}).Run(rc, map[string]any{"src": "big.unit", "name": "x.service"}, "")
		}},
	}
	for _, c := range cases {
		res := c.run()
		if res == nil || !res.Failed || !strings.Contains(res.Msg, "exceeds the") {
			t.Fatalf("%s 的本地读取应超限失败: %+v", c.name, res)
		}
	}
}

// TestSystemdUnitRejectsEmptyContent 空 content 不再部署空 unit 文件。
func TestSystemdUnitRejectsEmptyContent(t *testing.T) {
	rc, _ := newTestRC(t)
	m := &SystemdUnitModule{}
	res := m.Run(rc, map[string]any{"name": "x.service", "content": ""}, "")
	if res == nil || !res.Failed {
		t.Fatalf("空 content 应报错, got %+v", res)
	}
}

// TestParseBoolModel 严格布尔解析（inventory/playbook 共用）。
func TestParseBoolModel(t *testing.T) {
	if b, err := model.ParseBool("yes"); err != nil || !b {
		t.Fatalf("yes → true: %v", err)
	}
	if b, err := model.ParseBool("true"); err != nil || !b {
		t.Fatalf("true → true: %v", err)
	}
	if b, err := model.ParseBool("no"); err != nil || b {
		t.Fatalf("no → false: %v", err)
	}
	if _, err := model.ParseBool("maybe"); err == nil {
		t.Fatal("maybe 应报错")
	}
	if b, err := model.ParseBool(1); err != nil || !b {
		t.Fatalf("1 → true: %v", err)
	}
	if b, err := model.ParseBool(float64(0)); err != nil || b {
		t.Fatalf("0.0 → false: %v", err)
	}
}
