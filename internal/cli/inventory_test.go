package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/inventory"
)

// TestRunInventoryList 清单查看：主机表含地址/连接/分组，--vars 输出合并变量。
func TestRunInventoryList(t *testing.T) {
	dir := t.TempDir()
	invPath := filepath.Join(dir, "inv.yaml")
	os.WriteFile(invPath, []byte(`all:
  hosts:
    local1: {conn: local}
web:
  hosts:
    web1: {host: 10.0.0.11, conn: agent}
    web2: {host: 10.0.0.12}
`), 0o644)
	inv, err := inventory.Load(invPath)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runInventoryList(inv, "all", false, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"web1", "10.0.0.11", "agent", "web", "local1", "3 host(s)"} {
		if !strings.Contains(s, want) {
			t.Fatalf("输出应含 %q:\n%s", want, s)
		}
	}

	var vout bytes.Buffer
	if err := runInventoryList(inv, "web1", true, &vout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(vout.String(), "web1") {
		t.Fatalf("vars 输出应含主机名:\n%s", vout.String())
	}
}
