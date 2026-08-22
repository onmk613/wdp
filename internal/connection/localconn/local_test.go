package localconn

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/connection"
	"wdp/internal/model"
)

// newLocal 构造一个本地连接（直接实例化，绕过工厂）。
func newLocal() *Local { return &Local{host: "testhost"} }

// TestLocalExec 覆盖 Exec 三原语中的脚本执行：stdout/stderr 分流、
// stdin 传递、环境变量注入与退出码折叠（非零退出码不返回 error）。
func TestLocalExec(t *testing.T) {
	l := newLocal()
	ctx := context.Background()

	t.Run("echo 输出与退出码 0", func(t *testing.T) {
		res, err := l.Exec(ctx, connection.ExecRequest{Script: "echo hello"})
		if err != nil {
			t.Fatalf("Exec 不应报错: %v", err)
		}
		if res.Code != 0 {
			t.Fatalf("退出码 = %d, 期望 0", res.Code)
		}
		if res.Stdout != "hello\n" {
			t.Fatalf("stdout = %q, 期望 %q", res.Stdout, "hello\n")
		}
		if res.Stderr != "" {
			t.Fatalf("stderr = %q, 期望空", res.Stderr)
		}
	})

	t.Run("非零退出码折叠进 Code", func(t *testing.T) {
		res, err := l.Exec(ctx, connection.ExecRequest{Script: "exit 3"})
		if err != nil {
			t.Fatalf("退出码应折叠进 Code 而非返回 error: %v", err)
		}
		if res.Code != 3 {
			t.Fatalf("退出码 = %d, 期望 3", res.Code)
		}
	})

	t.Run("stderr 单独分流", func(t *testing.T) {
		res, err := l.Exec(ctx, connection.ExecRequest{Script: "echo err >&2"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Stderr != "err\n" || res.Stdout != "" {
			t.Fatalf("stdout=%q stderr=%q, 期望 stderr 为 %q", res.Stdout, res.Stderr, "err\n")
		}
	})

	t.Run("stdin 传递", func(t *testing.T) {
		res, err := l.Exec(ctx, connection.ExecRequest{Script: "cat", Stdin: "abc"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Stdout != "abc" {
			t.Fatalf("stdout = %q, 期望 %q（stdin 未透传）", res.Stdout, "abc")
		}
	})

	t.Run("Env 设置", func(t *testing.T) {
		res, err := l.Exec(ctx, connection.ExecRequest{
			Script: "echo \"$WDP_TEST_ENV\"",
			Env:    map[string]string{"WDP_TEST_ENV": "val123"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Stdout != "val123\n" {
			t.Fatalf("stdout = %q, 期望 %q（Env 未注入）", res.Stdout, "val123\n")
		}
	})
}

// TestLocalUploadFile 覆盖 UploadFile：内容校验、默认/显式权限位、
// 目标父目录自动创建与临时文件原子改名（无残留）。
func TestLocalUploadFile(t *testing.T) {
	l := newLocal()
	ctx := context.Background()
	dir := t.TempDir()

	t.Run("内容与默认权限 0644", func(t *testing.T) {
		dst := filepath.Join(dir, "a.txt")
		if err := l.UploadFile(ctx, dst, strings.NewReader("hello world"), 0); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "hello world" {
			t.Fatalf("内容 = %q, 期望 %q", string(data), "hello world")
		}
		assertPerm(t, dst, 0o644)
	})

	t.Run("显式权限 0755", func(t *testing.T) {
		dst := filepath.Join(dir, "b.sh")
		if err := l.UploadFile(ctx, dst, strings.NewReader("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		assertPerm(t, dst, 0o755)
	})

	t.Run("目标父目录不存在时自动创建", func(t *testing.T) {
		dst := filepath.Join(dir, "nested", "deep", "c.txt")
		if err := l.UploadFile(ctx, dst, strings.NewReader("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "x" {
			t.Fatalf("内容 = %q, 期望 %q", string(data), "x")
		}
		if _, err := os.Stat(filepath.Dir(dst)); err != nil {
			t.Fatalf("父目录未创建: %v", err)
		}
	})

	t.Run("原子性：无残留临时文件", func(t *testing.T) {
		sub := filepath.Join(dir, "atomic")
		dst := filepath.Join(sub, "d.txt")
		if err := l.UploadFile(ctx, dst, strings.NewReader("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		matches, err := filepath.Glob(filepath.Join(sub, ".wdp-upload-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Fatalf("目录残留临时文件: %v", matches)
		}
	})
}

// assertPerm 断言文件权限位。
func assertPerm(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s 权限 = %o, 期望 %o", path, got, want)
	}
}

// TestLocalDownloadFile 覆盖 DownloadFile：读回上传内容、缺失文件报错。
func TestLocalDownloadFile(t *testing.T) {
	l := newLocal()
	ctx := context.Background()
	dir := t.TempDir()

	src := filepath.Join(dir, "up.txt")
	if err := l.UploadFile(ctx, src, strings.NewReader("download-me"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := l.DownloadFile(ctx, src, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "download-me" {
		t.Fatalf("读回内容 = %q, 期望 %q", buf.String(), "download-me")
	}

	// 不存在的文件应报错
	if err := l.DownloadFile(ctx, filepath.Join(dir, "missing.txt"), &buf); err == nil {
		t.Fatal("下载不存在的文件应报错")
	}
}

// TestLocalConnectCloseHostname 验证 Connect/Close 幂等、Hostname 返回主机名。
func TestLocalConnectCloseHostname(t *testing.T) {
	l := newLocal()
	ctx := context.Background()

	for i := 0; i < 2; i++ { // Connect 幂等
		if err := l.Connect(ctx); err != nil {
			t.Fatalf("Connect 第 %d 次报错: %v", i+1, err)
		}
	}
	for i := 0; i < 2; i++ { // Close 幂等
		if err := l.Close(); err != nil {
			t.Fatalf("Close 第 %d 次报错: %v", i+1, err)
		}
	}
	if got := l.Hostname(); got != "testhost" {
		t.Fatalf("Hostname = %q, 期望 %q", got, "testhost")
	}
}

// TestLocalFactoryRegistered 验证 "local" 工厂已由 init 注册，且工厂产出的
// 连接 Hostname 取自主机名。
func TestLocalFactoryRegistered(t *testing.T) {
	c, err := connection.NewConnection(&model.Host{Name: "via-factory", Conn: "local"}, nil)
	if err != nil {
		t.Fatalf("local 工厂未注册: %v", err)
	}
	if got := c.Hostname(); got != "via-factory" {
		t.Fatalf("Hostname = %q, 期望 %q", got, "via-factory")
	}
}
