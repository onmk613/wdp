package chart

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildDeclTgz 构造仅含一个声明 size 字节的 chart.yaml 条目的 tgz（不写正文）。
func buildDeclTgz(t *testing.T, size int64) string {
	t.Helper()
	tgz := filepath.Join(t.TempDir(), "limitchart-0.1.0.tgz")
	f, err := os.Create(tgz)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "chart.yaml", Mode: 0o644, Size: size}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close() // 缺正文无关紧要：封顶检查在读正文前触发
	_ = gz.Close()
	return tgz
}

// TestLoadWithLimits 显式上限覆盖内置默认（wdp.cfg [transfer].max_extract_mb 注入路径）。
func TestLoadWithLimits(t *testing.T) {
	tgz := buildDeclTgz(t, 100)

	// 50 字节上限：100 字节条目被拒
	_, err := LoadWithLimits(tgz, Limits{MaxExtractBytes: 50})
	if err == nil {
		t.Fatal("自定义上限应拒绝超限包")
	}

	// 默认上限（2GiB）：不因上限报错（报的是缺正文等无关错误也算通过上限检查）
	if _, err := LoadWithLimits(tgz, Limits{}); err != nil && strings.Contains(err.Error(), "2147483648") {
		t.Fatalf("默认上限不应拒绝小包: %v", err)
	}

	// OpenWithLimits 走同一注入路径
	if _, _, _, err := OpenWithLimits(tgz, nil, nil, Limits{MaxExtractBytes: 50}); err == nil {
		t.Fatal("OpenWithLimits 应传递上限")
	}
}
