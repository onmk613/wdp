package cli

import (
	"testing"
	"time"

	"wdp/internal/release"
)

// rec 构造测试记录。
func rec(id, chart, version string) *release.Record {
	return &release.Record{ID: id, Chart: chart, Version: version, Time: time.Unix(1700000000, 0)}
}

// TestResolveRecordIDs 默认模式：精确优先、唯一前缀、歧义与零命中报错、去重保序。
func TestResolveRecordIDs(t *testing.T) {
	recs := []*release.Record{
		rec("myapp-1760001111222233330", "myapp", "1.0"),
		rec("myapp-1760002222333344440", "myapp", "1.1"),
		rec("other-1760003333444455550", "other", "2.0"),
	}

	// 精确 + 唯一前缀混用
	got, err := resolveRecordIDs(recs, []string{"other-1760003333444455550", "myapp-1760001111"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 || got[0].ID != "other-1760003333444455550" || got[1].ID != "myapp-1760001111222233330" {
		t.Errorf("解析结果不符: %+v", got)
	}

	// 歧义前缀
	if _, err := resolveRecordIDs(recs, []string{"myapp-17"}); err == nil {
		t.Errorf("歧义前缀应报错")
	}
	// 零命中
	if _, err := resolveRecordIDs(recs, []string{"nosuch"}); err == nil {
		t.Errorf("零命中应报错")
	}
	// 重复参数去重
	got, err = resolveRecordIDs(recs, []string{"other-1", "other-1760003333444455550"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("重复参数应去重，got %d", len(got))
	}
}

// TestSelectRecordPatterns 批量模式：前缀/正则多参数取并集、零命中与坏正则报错。
func TestSelectRecordPatterns(t *testing.T) {
	recs := []*release.Record{
		rec("myapp-1760001111222233330", "myapp", "1.0"),
		rec("myapp-1760002222333344440", "myapp", "1.1"),
		rec("other-1760003333444455550", "other", "2.0"),
	}

	// 前缀批量：命中 myapp 全部两条
	got, err := selectRecordPatterns(recs, []string{"myapp"}, false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("prefix myapp 应命中 2 条，got %d", len(got))
	}

	// 多参数并集：myapp 前缀 + other 精确 = 全部 3 条
	got, err = selectRecordPatterns(recs, []string{"myapp", "other-1760003333444455550"}, false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("并集应命中 3 条，got %d", len(got))
	}

	// 正则锚定前缀：等价于前缀批量
	got, err = selectRecordPatterns(recs, []string{"^myapp"}, true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("regex ^myapp 应命中 2 条，got %d", len(got))
	}

	// 正则非锚定：mid-string 命中（1760002 只在第二条 ID 中）
	got, err = selectRecordPatterns(recs, []string{"1760002"}, true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0].ID != "myapp-1760002222333344440" {
		t.Errorf("regex 1760002 应命中 1 条特定记录，got %+v", got)
	}

	// 零命中
	if _, err := selectRecordPatterns(recs, []string{"nothing"}, false); err == nil {
		t.Errorf("零命中应报错")
	}
	if _, err := selectRecordPatterns(recs, []string{"^nothing"}, true); err == nil {
		t.Errorf("regex 零命中应报错")
	}

	// 坏正则
	if _, err := selectRecordPatterns(recs, []string{"^[a"}, true); err == nil {
		t.Errorf("坏正则应报错")
	}
}
