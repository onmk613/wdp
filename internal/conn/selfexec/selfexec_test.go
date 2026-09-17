package selfexec

// selfexec 是 agent 自治执行的本地连接（become 语义与远程 agent 通道一致），
// 此前零覆盖。测试覆盖：工厂注册、Exec 透传、上传原子写与权限、下载。

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/conn"
	"wdp/internal/model"
)

func TestFactoryRegistered(t *testing.T) {
	h := &model.Host{Name: "n1", Conn: "selfexec", BecomePassword: "env:WDP_TEST_BECOME_PW"}
	t.Setenv("WDP_TEST_BECOME_PW", "s3cret")
	c, err := conn.NewConnection(h, nil)
	if err != nil {
		t.Fatalf("selfexec 应已注册: %v", err)
	}
	se, ok := c.(*SelfExec)
	if !ok {
		t.Fatalf("工厂应返回 *SelfExec，实际 %T", c)
	}
	if se.becomePassword != "s3cret" {
		t.Fatalf("become 密码应从 env 解析: %q", se.becomePassword)
	}
	if se.Hostname() != "n1" {
		t.Fatalf("Hostname = %q", se.Hostname())
	}
}

func TestExecPassthrough(t *testing.T) {
	se := New("local")
	if err := se.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, err := se.Exec(context.Background(), conn.ExecRequest{Script: "echo hello-selfexec"})
	if err != nil || !strings.Contains(out.Stdout, "hello-selfexec") {
		t.Fatalf("Exec 应透传输出: %+v err=%v", out, err)
	}
	if out.Code != 0 {
		t.Fatalf("退出码 = %d", out.Code)
	}
	// 非零退出码同样透传
	out, err = se.Exec(context.Background(), conn.ExecRequest{Script: "exit 7"})
	if err != nil || out.Code != 7 {
		t.Fatalf("退出码应透传: %+v err=%v", out, err)
	}
	if err := se.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUploadDownloadRoundTrip(t *testing.T) {
	se := New("local")
	dir := t.TempDir()
	dst := filepath.Join(dir, "sub", "file.txt")
	if err := se.UploadFile(context.Background(), dst, bytes.NewReader([]byte("payload")), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("上传应保留权限 0600，实际 %04o", perm)
	}
	// 上传目录不应残留临时文件
	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("目录应只剩目标文件: %v", entries)
	}
	var buf bytes.Buffer
	if err := se.DownloadFile(context.Background(), dst, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "payload" {
		t.Fatalf("下载内容 = %q", buf.String())
	}
	if err := se.DownloadFile(context.Background(), filepath.Join(dir, "nope"), &buf); err == nil {
		t.Fatal("缺失文件应报错")
	}
}
