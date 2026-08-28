package chart

// inventory_override 白名单的 lint 校验：键必须是 values 顶层键（合并 -f/--set 后判定）。

import (
	"strings"
	"testing"
)

func TestLintInventoryOverride(t *testing.T) {
	c := &Chart{Meta: Meta{Name: "t", InventoryOverride: []string{"tier", "nope"}}}
	values := map[string]any{"tier": "silver"}

	var warnCount int
	for _, is := range Lint(c, values) {
		if strings.Contains(is.Msg, "tier") && !strings.Contains(is.Msg, "nope") {
			t.Fatalf("values 存在的键不应告警: %s", is.Msg)
		}
		if is.Level == WARN && strings.Contains(is.Msg, "nope") {
			warnCount++
		}
	}
	if warnCount != 1 {
		t.Fatalf("不存在的键应告警一次, got %d", warnCount)
	}
}
