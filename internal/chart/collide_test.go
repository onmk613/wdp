package chart

import (
	"testing"

	"wdp/internal/model"
)

// TestValueCollisions 碰撞分类：独立键不报、同名未列入 → shadowed、
// 同名列入白名单 → overridden、内置键忽略。
func TestValueCollisions(t *testing.T) {
	values := map[string]any{"tier": "silver", "smart": false}
	hosts := []*model.Host{
		{Name: "h1", Vars: map[string]any{
			"tier":                "gold", // 同名，未列入 → shadowed
			"node_exporter_smart": true,   // 独立键 → 不参与
			"inventory_hostname":  "h1",   // 内置键 → 忽略
			"group_names":         []string{"webservers"},
		}},
		{Name: "h2", Vars: map[string]any{"tier": "bronze"}},
	}

	shadowed, overridden := ValueCollisions(values, hosts, nil)
	if len(overridden) != 0 {
		t.Fatalf("无白名单时不应有 overridden: %#v", overridden)
	}
	if len(shadowed["tier"]) != 2 {
		t.Fatalf("shadowed[tier] 应含两台主机: %#v", shadowed)
	}

	shadowed, overridden = ValueCollisions(values, hosts, []string{"tier"})
	if len(shadowed) != 0 {
		t.Fatalf("列入白名单后不应再 shadowed: %#v", shadowed)
	}
	if len(overridden["tier"]) != 2 {
		t.Fatalf("overridden[tier]: %#v", overridden)
	}

	// smart 键只在 values 有、主机没有 → 不构成碰撞
	if _, ok := shadowed["smart"]; ok {
		t.Fatalf("单侧存在不构成碰撞: %#v", shadowed)
	}
}
