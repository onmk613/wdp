package pushbin

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// gzipped 把内容压成 gzip 字节流。
func gzipped(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := io.WriteString(gw, content); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum256(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// extractTo 的校验逻辑与构建模式无关，两种 tag 下都跑。
func TestExtractToVerifies(t *testing.T) {
	content := "#!/bin/sh\necho wdp\n"
	ok := payload{Platform: "test_x", SHA256: sum256(content), Size: int64(len(content)), Version: "t"}

	path, err := extractTo(ok, bytes.NewReader(gzipped(t, content)))
	if err != nil {
		t.Fatalf("合法载荷应解取成功: %v", err)
	}
	defer os.Remove(path)
	got, err := os.ReadFile(path)
	if err != nil || string(got) != content {
		t.Fatalf("解取内容不一致: %q, %v", got, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("解取产物应可执行: %v, %v", fi, err)
	}

	bad := ok
	bad.SHA256 = "deadbeef"
	if _, err := extractTo(bad, bytes.NewReader(gzipped(t, content))); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("哈希不匹配应报错: %v", err)
	}
	bad = ok
	bad.Size = bad.Size + 1
	if _, err := extractTo(bad, bytes.NewReader(gzipped(t, content))); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("字节数不匹配应报错: %v", err)
	}
}

// 内嵌集成校验：./build.sh push 生成 assets 后以 -tags pushembed 运行；
// 默认构建（无 tag）Platforms 为空，跳过。
func TestEmbeddedPayloads(t *testing.T) {
	plats := Platforms()
	if len(plats) == 0 {
		t.Skip("默认构建无内嵌载荷；./build.sh push 后以 -tags pushembed 重跑")
	}
	for _, p := range plats {
		path, err := Lookup(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: stat %v", p, err)
		}
		if fi.Mode()&0o111 == 0 {
			t.Fatalf("%s: 解取产物应可执行", p)
		}
		if fi.Size() < 1<<20 {
			t.Fatalf("%s: 载荷异常偏小 (%d B)", p, fi.Size())
		}
	}
	if _, err := Lookup("linux_bogus"); !errors.Is(err, ErrNotEmbedded) {
		t.Fatalf("未知平台应返回 ErrNotEmbedded: %v", err)
	}
}
