package module

// 本地路径沙箱回归：chart 由 operator 级账号上传/编辑（app:create /
// app:upload），src/cache 若可以是任意控制端路径，一句
//   copy: {src: ../../../../home/u/.ssh/id_rsa, dest: /tmp/x}
// 就能把控制端私钥、~/.wdp/releases/*.json（含 values 明文）分发到目标机；
// artifact 的 cache 落盘方向还能反过来**写**控制端任意文件。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveLocalSandbox 绝对路径与 .. 逃逸都被拒绝，chart 内路径正常解析。
func TestResolveLocalSandbox(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	rc := &RunContext{BaseDir: base}

	// 正常：相对路径（含子目录）
	got, err := resolveLocal(rc, "templates/app.conf")
	if err != nil {
		t.Fatalf("chart 内相对路径应可解析: %v", err)
	}
	if got != filepath.Join(base, "templates", "app.conf") {
		t.Fatalf("解析结果异常: %s", got)
	}

	// 逃逸：.. 上跳
	for _, bad := range []string{
		"../../../../etc/passwd",
		"../sibling.txt",
		"templates/../../outside",
		"/etc/passwd",
		"/proc/self/environ",
	} {
		if _, err := resolveLocal(rc, bad); err == nil {
			t.Fatalf("越界路径应被拒绝: %s", bad)
		} else if !strings.Contains(err.Error(), "outside the chart/playbook directory") {
			t.Fatalf("错误信息应说明沙箱边界: %v", err)
		}
	}

	// 边界：与 BaseDir 前缀相同但不是子目录（/tmp/x vs /tmp/xy）
	sibling := base + "-evil"
	rc2 := &RunContext{BaseDir: base}
	if _, err := resolveLocal(rc2, sibling); err == nil {
		t.Fatalf("同前缀兄弟目录不算 chart 内: %s", sibling)
	}
}

// TestCopyRejectsEscape 模块层：越界 src 必须失败（而不是把文件发出去）。
func TestCopyRejectsEscape(t *testing.T) {
	rc, _ := newTestRC(t)
	rc.BaseDir = t.TempDir()
	res := (&CopyModule{}).Run(rc, map[string]any{
		"src":  "../../../../etc/hosts",
		"dest": "/tmp/leak",
	}, "")
	if res == nil || !res.Failed {
		t.Fatalf("越界 src 应失败: %+v", res)
	}
	if !strings.Contains(res.Msg, "outside the chart/playbook directory") {
		t.Fatalf("失败原因应指向沙箱: %q", res.Msg)
	}
}
