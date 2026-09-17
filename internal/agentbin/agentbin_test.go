package agentbin

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFromUname uname -sm 输出归一为 os_arch 平台键。
func TestFromUname(t *testing.T) {
	cases := map[string]string{
		"Linux x86_64\n": "linux_amd64",
		"Linux aarch64":  "linux_arm64",
		"Linux arm64\n":  "linux_arm64",
		"Darwin arm64\n": "darwin_arm64",
		"Darwin x86_64":  "darwin_amd64",
		"Linux i686":     "linux_386",
		"SunOS sun4u":    "",
		"":               "",
		"Linux":          "",
		"weird a b":      "",
	}
	for in, want := range cases {
		if got := FromUname(in); got != want {
			t.Fatalf("FromUname(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFileName 平台键 → bin 目录产物文件名。
func TestFileName(t *testing.T) {
	cases := map[string]string{
		"linux_amd64":   "wdp-linux-amd64",
		"darwin_arm64":  "wdp-darwin-arm64",
		"windows_amd64": "wdp-windows-amd64.exe",
	}
	for in, want := range cases {
		if got := FileName(in); got != want {
			t.Fatalf("FileName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSiblingPathFor 同级查找：命中、未命中、windows 后缀、符号链接
// 解析、目录伪装与非法平台键。
func TestSiblingPathFor(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "wdp-darwin-arm64")
	touch := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	touch("wdp-darwin-arm64")
	touch("wdp-linux-amd64")
	touch("wdp-windows-amd64.exe")
	if err := os.Mkdir(filepath.Join(dir, "wdp-linux-386"), 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	if p, ok := SiblingPathFor(exe, "linux_amd64"); !ok || p != filepath.Join(real, "wdp-linux-amd64") {
		t.Fatalf("linux_amd64 应命中同级文件: %q %v", p, ok)
	}
	// windows 平台带 .exe 后缀（无后缀文件不算命中）
	if p, ok := SiblingPathFor(exe, "windows_amd64"); !ok || p != filepath.Join(real, "wdp-windows-amd64.exe") {
		t.Fatalf("windows_amd64 应命中 .exe: %q %v", p, ok)
	}
	if _, ok := SiblingPathFor(exe, "windows_arm64"); ok {
		t.Fatal("不存在的 windows 平台应未命中")
	}
	if _, ok := SiblingPathFor(exe, "linux_386"); ok {
		t.Fatal("目录伪装的同级名不应命中")
	}
	if _, ok := SiblingPathFor(exe, "linux_riscv64"); ok {
		t.Fatal("不存在的平台应未命中")
	}
	// 非法平台键（路径穿越等）一律拒绝
	for _, bad := range []string{"", "../evil", "linux/amd64", "Linux_amd64", "_linux"} {
		if _, ok := SiblingPathFor(exe, bad); ok {
			t.Fatalf("非法平台键 %q 不应命中", bad)
		}
	}
	// 入口不存在 → 未命中
	if _, ok := SiblingPathFor(filepath.Join(dir, "nope"), "linux_amd64"); ok {
		t.Fatal("入口不存在应未命中")
	}

	// 符号链接入口：解析到真实目录后命中同级文件
	deep := t.TempDir()
	realExe := filepath.Join(deep, "wdp-darwin-arm64")
	if err := os.WriteFile(realExe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "wdp-linux-amd64"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	outer := t.TempDir()
	link := filepath.Join(outer, "wdp")
	if err := os.Symlink(realExe, link); err != nil {
		t.Fatal(err)
	}
	if _, ok := SiblingPathFor(link, "linux_amd64"); !ok {
		t.Fatal("符号链接入口应解析到真实目录命中同级文件")
	}
}
