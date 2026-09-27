package release

import (
	"fmt"
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

// TestSaveConflictRetryIDConsistent 冲突重试时文件名、正文 id、返回值
// 三者一致：正文必须在序号后缀定型之后序列化；重试有上限（连撞 8 次报
// 明确错误而非无限循环）。
func TestSaveConflictRetryIDConsistent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	relDir := filepath.Join(home, ".wdp", "releases")
	if err := os.MkdirAll(relDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := time.Unix(1700000000, 0)
	base := "app-1700000000000000000"

	// 预占基名文件：Save 应追加 -1 重试
	if err := os.WriteFile(filepath.Join(relDir, base+".json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := Save(&Record{Chart: "app", Hosts: []string{"h1"}, Time: ts})
	if err != nil {
		t.Fatal(err)
	}
	if id != base+"-1" {
		t.Fatalf("冲突后应追加序号: %q", id)
	}
	rec, err := Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID != id {
		t.Fatalf("正文 id 应与文件名一致: 正文 %q, 文件名 %q", rec.ID, id)
	}

	// 预占全部 8 个候选名（attempt 0..7）：超限报错而非无限重试
	if err := os.Remove(filepath.Join(relDir, id+".json")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		n := base + ".json"
		if i > 0 {
			n = fmt.Sprintf("%s-%d.json", base, i)
		}
		if err := os.WriteFile(filepath.Join(relDir, n), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if id, err := Save(&Record{Chart: "app", Hosts: []string{"h1"}, Time: ts}); err == nil {
		t.Fatalf("连撞 %d 次应报错, 却返回 %q", 8, id)
	}
}

// TestDelete 删除记录：删后不可读，重复删除与不存在的记录报错。
func TestDelete(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	id, err := Save(&Record{Chart: "app", Version: "1.0", Hosts: []string{"h1"}, Time: time.Unix(1700000000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := Delete(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := Load(id); err == nil {
		t.Errorf("删除后记录应不可读")
	}
	if err := Delete(id); err == nil {
		t.Errorf("重复删除应报错")
	}
	if err := Delete("no-such-record"); err == nil {
		t.Errorf("删除不存在的记录应报错")
	}
}
