package pushcerts

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wdp/internal/ca"
	"wdp/internal/conn"
)

// testDC 返回测试专用默认值：显式临时会话目录 + 30 分钟轮换
// （绝不落到真实 ~/.wdp/push-ca）。
func testDC(t *testing.T) *conn.Defaults {
	t.Helper()
	return &conn.Defaults{AgentCertRotateMin: 30, PushCADir: t.TempDir()}
}

// TestRotationStore 验证代数仓库：默认不轮换；到期换新代且新旧信任链独立。
func TestRotationStore(t *testing.T) {
	Reset()

	// 默认（不轮换）：多次取值同代（同一 dc 同一会话目录）
	dc0 := testDC(t)
	c1, g1, err := Material(dc0)
	if err != nil || g1 != 1 {
		t.Fatalf("首代应为 gen=1: gen=%d err=%v", g1, err)
	}
	if _, g2, _ := Material(dc0); g2 != 1 {
		t.Fatalf("未配置轮换不应换代: gen=%d", g2)
	}

	// 配置 30 分钟，回拨时钟 31 分钟 → 换新代，信任链独立
	dc2 := testDC(t)
	store.Lock()
	store.dir, store.at = "", time.Now().Add(-31*time.Minute)
	store.Unlock()
	c3, g3, err := Material(dc2)
	if err != nil || g3 != 2 {
		t.Fatalf("到期应换为 gen=2: gen=%d err=%v", g3, err)
	}
	pool1 := x509.NewCertPool()
	pool1.AppendCertsFromPEM(c1.CACertPEM)
	block, _ := pem.Decode(c3.ServerCertPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool1, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("新旧代信任链应相互独立")
	}

	// 未到期 → 不换
	if _, g4, _ := Material(dc2); g4 != 2 {
		t.Fatalf("未到期不应换代: gen=%d", g4)
	}
}

// TestSessionCAOnDisk 会话 CA 落盘：产物为普通 wdp ca 文件（ca show 可检视），
// 有效期内复用同一信任链；CA 丢失/损坏时自动重建且新链独立。
func TestSessionCAOnDisk(t *testing.T) {
	dir := t.TempDir()
	c1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 产物齐备且可经 ca.LoadCAAt 检视（与 wdp ca show 同路径）
	if _, _, err := ca.LoadCAAt(filepath.Join(dir, ca.DefaultCAFile), filepath.Join(dir, ca.DefaultKeyFile)); err != nil {
		t.Fatalf("会话 CA 应可被 ca 工具加载: %v", err)
	}
	for _, f := range []string{pushServerName + ".crt", pushControlName + ".crt"} {
		if _, err := parseCertFile(filepath.Join(dir, f)); err != nil {
			t.Fatalf("会话叶子证书 %s 应存在: %v", f, err)
		}
	}
	// 复用：同目录再次加载，CA 与叶子不变
	c2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(c1.CACertPEM) != string(c2.CACertPEM) ||
		string(c1.ServerCertPEM) != string(c2.ServerCertPEM) ||
		string(c1.ClientKeyPEM) != string(c2.ClientKeyPEM) {
		t.Fatal("有效期内应复用同一信任链与证书对")
	}
	// CA 损坏 → 自动重建，新链独立
	if err := os.Remove(filepath.Join(dir, ca.DefaultCAFile)); err != nil {
		t.Fatal(err)
	}
	c3, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(c3.CACertPEM) == string(c1.CACertPEM) {
		t.Fatal("重建后应为全新信任链")
	}
	pool1 := x509.NewCertPool()
	pool1.AppendCertsFromPEM(c1.CACertPEM)
	block, _ := pem.Decode(c3.ServerCertPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool1}); err == nil {
		t.Fatal("新旧信任链应相互独立")
	}
}
