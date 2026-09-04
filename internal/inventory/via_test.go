package inventory

// §7.4 via 中继：链式可达声明、词法解析与成环检测（加载期拦截）。

import (
	"strings"
	"testing"
)

const viaInv = `
all:
  hosts:
    bastion-1: {ansible_host: 10.0.0.1}
    jump-2: {ansible_host: 10.20.1.2}
prod-inner:
  vars:
    via: bastion-1
  hosts:
    10.20.0.11: {}
    10.20.0.12: {}
deep-inner:
  vars:
    via: jump-2
  hosts:
    10.30.0.21: {}
jump-relay:
  vars:
    via: bastion-1
  hosts:
    jump-2: {}
direct:
  hosts:
    10.40.0.31: {}
`

func loadVia(t *testing.T) *Inventory {
	t.Helper()
	inv, err := Parse([]byte(viaInv))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestViaChainAndRoot(t *testing.T) {
	inv := loadVia(t)
	// 无声明：根即自身，链为空
	if chain := inv.ViaChain("10.40.0.31"); len(chain) != 0 || inv.ViaRoot("10.40.0.31") != "10.40.0.31" {
		t.Fatalf("直连主机不应有链: %v", chain)
	}
	// 单跳
	if chain := inv.ViaChain("10.20.0.11"); len(chain) != 1 || chain[0] != "bastion-1" {
		t.Fatalf("单跳链: %v", chain)
	}
	if inv.ViaRoot("10.20.0.11") != "bastion-1" {
		t.Fatal("单跳根")
	}
	// 链式：deep-inner → jump-2 → bastion-1
	if chain := inv.ViaChain("10.30.0.21"); len(chain) != 2 || chain[0] != "jump-2" || chain[1] != "bastion-1" {
		t.Fatalf("链式: %v", chain)
	}
	if inv.ViaRoot("10.30.0.21") != "bastion-1" {
		t.Fatal("链式根")
	}
}

func TestViaCycleRejected(t *testing.T) {
	src := `
all:
  hosts: {ha: {}, hb: {}}
g1:
  vars: {via: hb}
  hosts: {ha: {}}
g2:
  vars: {via: ha}
  hosts: {hb: {}}
`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "via cycle") {
		t.Fatalf("成环应加载期报错: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "ha -> hb") {
		t.Fatalf("错误应列出环路: %v", err)
	}
}

func TestViaUnknownTargetRejected(t *testing.T) {
	_, err := Parse([]byte("g:\n  vars: {via: no-such-host}\n  hosts: {h1: {}}\n"))
	if err == nil || !strings.Contains(err.Error(), "not in the inventory") {
		t.Fatalf("未知名应报错: %v", err)
	}
}
