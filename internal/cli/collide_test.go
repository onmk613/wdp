package cli

import (
	"bytes"
	"strings"
	"testing"

	"wdp/internal/model"
)

// 碰撞分类规则在 internal/chart（chart.ValueCollisions，见其包内测试）；
// 此处只测命令层的告警呈现。

// TestReportValueCollisions 输出包含修复提示（独立键名 + dig 或白名单）。
func TestReportValueCollisions(t *testing.T) {
	var buf bytes.Buffer
	values := map[string]any{"tier": "silver"}
	hosts := []*model.Host{{Name: "h1", Vars: map[string]any{"tier": "gold"}}}
	reportValueCollisions(&buf, values, hosts, nil)
	out := buf.String()
	if !strings.Contains(out, "shadowed by chart values") || !strings.Contains(out, "inventory_override") {
		t.Fatalf("告警缺少提示:\n%s", out)
	}

	buf.Reset()
	reportValueCollisions(&buf, values, hosts, []string{"tier"})
	if !strings.Contains(buf.String(), "inventory override active") {
		t.Fatalf("白名单生效应打信息:\n%s", buf.String())
	}
}
