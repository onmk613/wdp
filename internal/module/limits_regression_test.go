package module

// 读取封顶回归：控制端内存不得被"归档解压放大 / 超大本地文件 /
// 超大远端文件"打爆。
//
// 这些都是"控制端进程内"的读取路径（web 控制台经 console 同进程执行），
// 一旦 OOM 会连整个控制台一起带走；此前各路径口径不一：
// members 选取消费 io.ReadAll 无上限、artifact 缓存裸 os.ReadFile、
// lineinfile 与 contentDiff 先整份下载再判上限。

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bombTarGz 构造"小体积、大解压"的归档：n 字节零值压缩后极小。
func bombTarGz(t *testing.T, name string, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(n), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestSelectArchiveMembersRejectsBomb 成员解压后超过上限即报错（而不是
// 先把几百 MB 读进内存再说）。
func TestSelectArchiveMembersRejectsBomb(t *testing.T) {
	const limit = 1 << 16
	arc := bombTarGz(t, "big/app", 8<<20) // 解压 8 MiB，压缩后仅几 KB
	if _, err := selectArchiveMembers("targz", arc, []string{"app"}, limit); err == nil {
		t.Fatal("超过成员上限的条目应报错")
	} else if !strings.Contains(err.Error(), "member limit") {
		t.Fatalf("错误信息应指明成员上限: %v", err)
	}

	// 上限内的成员照常可取（避免误杀）
	small := buildTarGz(t, map[string]string{"app": "OK"})
	sel, err := selectArchiveMembers("targz", small, []string{"app"}, limit)
	if err != nil || len(sel) != 1 || string(sel[0].data) != "OK" {
		t.Fatalf("上限内成员应正常选取: %v %+v", err, sel)
	}
}

// TestSelectArchiveMembersRejectsBombZip zip 侧按目录声明大小先判。
func TestSelectArchiveMembersRejectsBombZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, 4<<20)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := selectArchiveMembers("zip", buf.Bytes(), []string{"app"}, 1<<16); err == nil {
		t.Fatal("zip 成员超限应报错")
	}
}

// TestReadLocalCapRejectsOversize 本地读取超限 fail-loud 且报出上限值。
func TestReadLocalCapRejectsOversize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(p, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	rc := &RunContext{}
	if _, err := readLocalCap(rc, p, 1024); err == nil {
		t.Fatal("超过上限的本地文件应报错")
	} else if !strings.Contains(err.Error(), "max_upload_mb") {
		t.Fatalf("错误信息应指向配置项: %v", err)
	}
	// 上限内正常
	got, err := readLocalCap(rc, p, 8192)
	if err != nil || len(got) != 4096 {
		t.Fatalf("上限内应正常读取: %v %d", err, len(got))
	}
}

// TestCappedBufferTruncates cappedBuffer 超限后继续接收但不再增长，
// 且能报告"发生过截断"（供调用方降级提示或 fail-loud）。
func TestCappedBufferTruncates(t *testing.T) {
	b := &cappedBuffer{max: 100}
	n, err := b.Write(bytes.Repeat([]byte("x"), 1000))
	if err != nil || n != 1000 {
		t.Fatalf("Write 应吞下全部输入（不中断上游）: %d %v", n, err)
	}
	if b.buf.Len() != 100 {
		t.Fatalf("缓冲应停在上限: %d", b.buf.Len())
	}
	if !b.truncated() {
		t.Fatal("应报告发生截断")
	}
	b2 := &cappedBuffer{max: 100}
	_, _ = b2.Write([]byte("short"))
	if b2.truncated() {
		t.Fatal("未超限不应报告截断")
	}
}

// TestContentDiffCapsDownload 远端文件超过 diff 上限时只提示、不整份读入。
func TestContentDiffCapsDownload(t *testing.T) {
	rc, f := newTestRC(t)
	dest := "/etc/app.conf"
	f.Files[dest] = []byte(strings.Repeat("y", maxDiffBytes*3)) // 3 MiB，超过 1 MiB 上限

	got := contentDiff(rc, dest, true, "want\n")
	if !strings.Contains(got, "diff limit") {
		t.Fatalf("超限应降级为提示而非整份 diff: %q", got)
	}
}

// TestLineinfileRejectsOversizeRemote 远端文件超限时 fail-loud（截断后
// 回写会改坏远端文件，不能静默降级）。
func TestLineinfileRejectsOversizeRemote(t *testing.T) {
	rc, f := newTestRC(t)
	rc.MaxUploadBytes = 1 << 10
	path := "/etc/hosts"
	f.Files[path] = []byte(strings.Repeat("z", 4096))

	res := (&LineinfileModule{}).Run(rc, map[string]any{
		"path": path, "line": "127.0.0.1 localhost",
	}, "")
	if res == nil || !res.Failed {
		t.Fatalf("超限应失败: %+v", res)
	}
	if !strings.Contains(res.Msg, "max_upload_mb") {
		t.Fatalf("错误信息应指向配置项: %q", res.Msg)
	}
}
