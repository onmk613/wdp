package inventory

import (
	"testing"

	"wdp/internal/model"
)

// 回归：菱形组成员（parent→{a,b}，主机同属 a、b）时子组变量必须
// 覆盖父组（文档承诺 all < 父组 < 子组）。此前 appendUnique 移尾会把
// parent 排到子组之后，父组变量静默获胜。
func TestDiamondMembershipChildWins(t *testing.T) {
	inv := &Inventory{
		Groups: map[string]*model.Group{
			"parent": {Vars: map[string]any{"x": "from_parent"}, Children: []string{"a", "b"}},
			"a":      {Vars: map[string]any{"x": "from_a"}, HostNames: []string{"h1"}},
			"b":      {Vars: map[string]any{}, HostNames: []string{"h1"}},
		},
		Hosts: []*model.Host{{Name: "h1"}},
	}
	m := inv.groupMembership()
	got := m["h1"]
	// parent 必须在 a、b 之前（拓扑序：祖先先行）
	pos := map[string]int{}
	for i, g := range got {
		pos[g] = i
	}
	if pos["parent"] > pos["a"] || pos["parent"] > pos["b"] {
		t.Fatalf("菱形下父组排到了子组之后: %v", got)
	}

	inv.applyVars()
	if v := inv.Hosts[0].Vars["x"]; v != "from_a" {
		t.Fatalf("子组变量应获胜, got %v", v)
	}
}

// 深链仍保持父→子顺序（gp < mid < leaf，leaf 变量获胜）。
func TestChainMembershipOrder(t *testing.T) {
	inv := &Inventory{
		Groups: map[string]*model.Group{
			"gp":   {Vars: map[string]any{"x": "gp"}, Children: []string{"mid"}},
			"mid":  {Vars: map[string]any{"x": "mid"}, Children: []string{"leaf"}},
			"leaf": {Vars: map[string]any{"x": "leaf"}, HostNames: []string{"h1"}},
		},
		Hosts: []*model.Host{{Name: "h1"}},
	}
	inv.applyVars()
	if v := inv.Hosts[0].Vars["x"]; v != "leaf" {
		t.Fatalf("深链叶子变量应获胜, got %v", v)
	}
}
