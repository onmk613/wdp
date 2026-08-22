package release

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSaveLoadContainedInDir 记录 ID 含 ../ 穿越时（ID 源自 chart 名），
// Save/Load 的读写均被 securejoin 收敛在 releases 目录内，不越出。
func TestSaveLoadContainedInDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	relDir := filepath.Join(home, ".wdp", "releases")
	ts := time.Unix(1700000000, 0) // 秒+纳秒固定时间：ID 取 UnixNano

	id, err := Save(&Record{Chart: "../evil", Hosts: []string{"h1"}, Time: ts})
	if err != nil {
		t.Fatal(err)
	}
	if id != "../evil-1700000000000000000" {
		t.Fatalf("id = %q", id)
	}
	// 收敛后的落盘位置在 releases 内; 未越出到 ~/.wdp 之外
	if _, err := os.Stat(filepath.Join(relDir, "evil-1700000000000000000.json")); err != nil {
		t.Errorf("记录应被收敛写入 releases 目录: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "evil-1700000000000000000.json")); err == nil {
		t.Errorf("记录越出了 releases 目录")
	}

	// 穿越写法的 Load 同样被收敛: 解析到同一文件而非目录外
	rec, err := Load("../evil-1700000000000000000")
	if err != nil {
		t.Fatalf("收敛后的记录应可回读: %v", err)
	}
	if rec.Chart != "../evil" {
		t.Errorf("回读内容不符: %+v", rec)
	}
}
