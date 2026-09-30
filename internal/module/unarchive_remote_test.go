package module

// unarchive remote_src + members：目标机归档按名抽取拍平（docker 静态包
// 场景）。行为测试用 local 连接（真 sh/tar）+ 程序构造的归档端到端：
// 抽取（含权限）/幂等/未命中失败。

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/conn"
	_ "wdp/internal/conn/local" // 注册 local 连接工厂（端到端真 sh 执行）
	"wdp/internal/model"
)

// buildDockerTgz 构造 docker 静态包形态的归档（顶层 docker/ 目录，成员
// 带执行位），返回归档路径。
func buildDockerTgz(t *testing.T, members ...string) string {
	t.Helper()
	arcPath := filepath.Join(t.TempDir(), "docker.tgz")
	f, err := os.Create(arcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	for _, m := range members {
		hdr := &tar.Header{Name: "docker/" + m, Mode: 0o755, Size: int64(len("bin-" + m))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("bin-" + m)); err != nil {
			t.Fatal(err)
		}
	}
	return arcPath
}

func newLocalRC(t *testing.T) *RunContext {
	t.Helper()
	lc, err := conn.NewConnection(&model.Host{Name: "local-test", Conn: "local"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &RunContext{Ctx: context.Background(), Conn: lc, Host: &model.Host{Name: "local-test"}, Vars: map[string]any{}}
}

// TestUnarchiveRemoteMembers 远端 members：按 basename 拍平到 dest、权限
// 沿用归档条目、二次执行幂等（内容一致不 changed）。
func TestUnarchiveRemoteMembers(t *testing.T) {
	arc := buildDockerTgz(t, "dockerd", "docker", "containerd", "runc")
	dest := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	rc := newLocalRC(t)
	mod := &UnarchiveModule{}

	r1 := mod.Run(rc, map[string]any{
		"src": arc, "dest": dest, "remote_src": true,
		"members": []any{"dockerd", "containerd"},
	}, "")
	if r1.Failed {
		t.Fatalf("远端 members 抽取失败: %s", r1.Msg)
	}
	if !r1.Changed {
		t.Fatal("首次抽取应 changed")
	}
	for _, m := range []string{"dockerd", "containerd"} {
		data, err := os.ReadFile(filepath.Join(dest, m))
		if err != nil || string(data) != "bin-"+m {
			t.Fatalf("成员 %s 内容异常: %v", m, err)
		}
		st, _ := os.Stat(filepath.Join(dest, m))
		if st != nil && st.Mode().Perm() != 0o755 {
			t.Fatalf("成员 %s 权限应沿用归档 0755: %v", m, st.Mode())
		}
	}
	// 未选中的成员不落盘
	if _, err := os.Stat(filepath.Join(dest, "runc")); !os.IsNotExist(err) {
		t.Fatal("未选中的成员不应落盘")
	}

	// 幂等：内容一致 → 不 changed
	r2 := mod.Run(rc, map[string]any{
		"src": arc, "dest": dest, "remote_src": true,
		"members": []any{"dockerd", "containerd"},
	}, "")
	if r2.Failed || r2.Changed {
		t.Fatalf("二次执行应幂等: %+v", r2)
	}

	// 未命中成员：失败且指明
	r3 := mod.Run(rc, map[string]any{
		"src": arc, "dest": dest, "remote_src": true,
		"members": []any{"no-such-bin"},
	}, "")
	if !r3.Failed || !strings.Contains(r3.Msg, "member") {
		t.Fatalf("未命中成员应失败: %+v", r3)
	}
}

// TestUnarchiveRemoteWhole 整包 remote_src 回归（既有能力）：解压到 dest
// 保留归档内部结构。
func TestUnarchiveRemoteWhole(t *testing.T) {
	arc := buildDockerTgz(t, "dockerd")
	dest := filepath.Join(t.TempDir(), "out")
	rc := newLocalRC(t)
	r := (&UnarchiveModule{}).Run(rc, map[string]any{"src": arc, "dest": dest, "remote_src": true}, "")
	if r.Failed {
		t.Fatalf("整包远端解压失败: %s", r.Msg)
	}
	if _, err := os.Stat(filepath.Join(dest, "docker", "dockerd")); err != nil {
		t.Fatalf("应保留归档目录结构: %v", err)
	}
}
