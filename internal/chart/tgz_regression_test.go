package chart

// chart tgz 符号链接安全回归：解包时链接目标必须收敛在解包根内。
// 背景：loadTgz 曾用 strings.TrimPrefix(link, "/") 剥前导斜杠，遇到
// "//etc/passwd" 只剥掉一个，残留的 "/etc/passwd" 会在解包根内建出
// 指向归档外的绝对链接；控制台读 spec（console.ReadSpecFromDir）与
// 保存副本（console.CopyDir）都跟随链接，等于服务端任意文件读取
// （可读到 /proc/self/environ 与 <data>/ca/ca.key）。

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTgzWithLink 生成只含最小合法 chart 与一个符号链接条目的 tgz。
func writeTgzWithLink(t *testing.T, linkName, linkTarget string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evil-0.1.0.tgz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	add := func(hdr *tar.Header, body string) {
		t.Helper()
		hdr.ModTime = hdr.ModTime.UTC()
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	add(&tar.Header{Name: "evil/chart.yaml", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len("name: evil\nversion: 0.1.0\n"))}, "name: evil\nversion: 0.1.0\n")
	add(&tar.Header{Name: "evil/values.yaml", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len("a: 1\n"))}, "a: 1\n")
	// deploy.yaml 是 chart 的必需文件（loadDir 要求）；目录条目也要显式
	// 给出——解包不给符号链接条目补建父目录（与真实 tar 一致）
	deploy := "- name: p\n  hosts: all\n  tasks: []\n"
	add(&tar.Header{Name: "evil/deploy.yaml", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(deploy))}, deploy)
	for _, d := range []string{"evil/templates", "evil/templates/sub"} {
		add(&tar.Header{Name: d + "/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	}
	add(&tar.Header{Name: linkName, Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: linkTarget}, "")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadTgzRejectsAbsoluteSymlink 绝对链接目标（含 "//" 变体，旧的
// TrimPrefix 实现正被它绕过）必须让整个包加载失败。
func TestLoadTgzRejectsAbsoluteSymlink(t *testing.T) {
	for _, target := range []string{"/etc/passwd", "//etc/passwd", "///etc/passwd", "/../etc/passwd"} {
		p := writeTgzWithLink(t, "evil/leak", target)
		c, err := Load(p)
		if err == nil {
			if c != nil {
				c.Close()
			}
			t.Fatalf("绝对链接目标 %q 应拒绝加载", target)
		}
		if !strings.Contains(err.Error(), "absolute symlink target") {
			t.Fatalf("错误信息应指明绝对链接, got %v", err)
		}
	}
}

// TestLoadTgzSkipsEscapingSymlink 相对但逃出解包根的链接：跳过该条目，
// 包仍可加载（不因无关链接阻断部署），且链接不得存在（否则读 spec
// 时会跟随它读到根外文件）。
func TestLoadTgzSkipsEscapingSymlink(t *testing.T) {
	for _, target := range []string{"../../../../etc/passwd", "../../../evil-out", "a/../../../../outside"} {
		p := writeTgzWithLink(t, "evil/templates/leak", target)
		c, err := Load(p)
		if err != nil {
			t.Fatalf("逃逸链接 %q 应被跳过而非让包加载失败: %v", target, err)
		}
		leak := filepath.Join(c.Dir, "templates", "leak")
		if _, err := os.Lstat(leak); err == nil {
			c.Close()
			t.Fatalf("逃逸链接 %q 不应被创建", target)
		}
		c.Close()
	}
}

// TestLoadTgzKeepsInRootRelativeSymlink 归一后仍在根内的相对链接保留：
// templates/sub/up.conf -> ../app.conf.tpl 是合法用法，不能被误杀。
func TestLoadTgzKeepsInRootRelativeSymlink(t *testing.T) {
	p := writeTgzWithLink(t, "evil/templates/sub/up.conf", "../../values.yaml")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("根内相对链接应保留: %v", err)
	}
	defer c.Close()
	link := filepath.Join(c.Dir, "templates", "sub", "up.conf")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("根内相对链接应存在: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("应是符号链接: %v", fi.Mode())
	}
	// 链接可解析且落在解包根内
	data, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("链接应可解析: %v", err)
	}
	if string(data) != "a: 1\n" {
		t.Fatalf("链接解析内容异常: %q", data)
	}
}
