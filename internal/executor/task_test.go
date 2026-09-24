package executor

import (
	"strings"
	"testing"

	"wdp/internal/model"
)

// TestAggregateLoopItemsCapped：Items 逐项输出受 16KiB 保留上限保护
// （万级 item × 1MiB 不受聚合 stdout 预算覆盖），且浅拷贝截断不改动
// register 引用的原结果对象。
func TestAggregateLoopItemsCapped(t *testing.T) {
	res := &model.TaskResult{}
	big := strings.Repeat("a", 20<<10)
	items := []*model.TaskResult{{Stdout: big, Stderr: big}, {Stdout: "ok"}}
	if stop := aggregateLoopResults(res, items); stop {
		t.Fatal("无不可达项不应终止")
	}
	lim := maxItemOutLen + 128 // 截断正文 + 标记行
	if len(res.Items[0].Stdout) > lim || len(res.Items[0].Stderr) > lim {
		t.Fatalf("Items 单项应封顶: stdout=%d stderr=%d", len(res.Items[0].Stdout), len(res.Items[0].Stderr))
	}
	if len(res.Items[1].Stdout) != 2 {
		t.Fatal("未超限项不应被截断")
	}
	if len(items[0].Stdout) != 20<<10 || len(items[0].Stderr) != 20<<10 {
		t.Fatal("截断不得改动原结果对象（register 引用共享）")
	}
}
