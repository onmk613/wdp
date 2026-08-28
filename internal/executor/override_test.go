package executor

// inventory_override 白名单回归：列入的 values 键允许 inventory 同名变量
// 反超（ansible 式语义按 chart 显式领取）；未列入维持"同名 values 恒赢"。
// 注意测试键不能选连接参数键（port/user 等不进入变量域），用 tier。

import (
	"context"
	"strings"
	"testing"

	"wdp/internal/chart"
	"wdp/internal/conn"
	"wdp/internal/inventory"
	"wdp/internal/model"
)

const overrideInv = `
webservers:
  hosts:
    h1: {conn: fake, tier: gold}
    h2: {conn: fake}
`

// TestInventoryOverrideAllowlisted 白名单键被 inventory 同名变量反超，
// 无该变量的主机维持 values 值；play vars 仍高于覆盖层。
func TestInventoryOverrideAllowlisted(t *testing.T) {
	ex, rep := setupFeature(t, false, func(host string, req conn.ExecRequest) (conn.ExecResult, error) {
		return conn.ExecResult{Code: 0, Stdout: "ran: " + req.Script + "\n"}, nil
	})
	// setupFeature 绑定固定 testInv；换用带 tier 变量的 inventory（fake 工厂全局生效）
	inv, err := inventory.Parse([]byte(overrideInv))
	if err != nil {
		t.Fatal(err)
	}
	ex2 := New(inv, ex.Conns, rep, Options{
		Forks:  2,
		Chart:  &chart.Chart{Meta: chart.Meta{InventoryOverride: []string{"tier"}}},
		Values: map[string]any{"tier": "silver"},
	})
	plays := []*model.Play{{
		Hosts: "webservers",
		Vars:  map[string]any{"play_var": "play-wins"},
		Tasks: []*model.Task{
			{Name: "验证覆盖", Module: "shell", FreeForm: "tier={{ .tier }} play={{ .play_var }}"},
		},
	}}
	if ex2.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	scripts := joinExecScripts(allFakes())
	// h1 有同名变量 tier=gold → 反超 values silver；h2 无变量 → 维持 silver
	if !strings.Contains(scripts, "tier=gold") {
		t.Fatalf("白名单覆盖未生效:\n%s", scripts)
	}
	if !strings.Contains(scripts, "tier=silver") {
		t.Fatalf("无变量的主机应维持 values 值:\n%s", scripts)
	}
	if !strings.Contains(scripts, "play=play-wins") {
		t.Fatalf("play vars 应高于覆盖层:\n%s", scripts)
	}
}

// TestInventoryOverrideNotListed 未列入白名单的同名键维持 values 恒赢。
func TestInventoryOverrideNotListed(t *testing.T) {
	ex, rep := setupFeature(t, false, func(host string, req conn.ExecRequest) (conn.ExecResult, error) {
		return conn.ExecResult{Code: 0, Stdout: "ran: " + req.Script + "\n"}, nil
	})
	inv, err := inventory.Parse([]byte(overrideInv))
	if err != nil {
		t.Fatal(err)
	}
	ex2 := New(inv, ex.Conns, rep, Options{
		Forks:  2,
		Chart:  &chart.Chart{Meta: chart.Meta{}}, // 未声明 inventory_override
		Values: map[string]any{"tier": "silver"},
	})
	plays := []*model.Play{{
		Hosts: "webservers",
		Tasks: []*model.Task{
			{Name: "验证默认语义", Module: "shell", FreeForm: "tier={{ .tier }}"},
		},
	}}
	if ex2.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	scripts := joinExecScripts(allFakes())
	if !strings.Contains(scripts, "tier=silver") || strings.Contains(scripts, "tier=gold") {
		t.Fatalf("未列入白名单时应维持 values 恒赢:\n%s", scripts)
	}
}
