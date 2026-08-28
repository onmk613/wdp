package report

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"wdp/internal/model"
)

// TestConsoleDiffHiddenUnderOutputNone output=none（含 no_log）时 diff 不回显，
// 兑现 docs/11「diff 展示遵循任务级 output 控制」。
func TestConsoleDiffHiddenUnderOutputNone(t *testing.T) {
	var buf bytes.Buffer
	c := NewConsole(&buf, false, 2) // -vv：全量输出级别

	c.PlayStart("p", []string{"h1"})
	c.TaskStart("敏感文件", "template")
	c.HostResult("h1", &model.TaskResult{
		Host: "h1", Changed: true, Output: "none",
		Diff: "+password=SECRET\n",
	})
	c.TaskDone()

	out := buf.String()
	if strings.Contains(out, "SECRET") {
		t.Fatalf("output=none 下 diff 泄漏:\n%s", out)
	}
	if !strings.Contains(out, "[output=none]") {
		t.Fatalf("应显示 [output=none] 标记:\n%s", out)
	}
}

// TestConsoleDiffShownByDefault 非 none 任务 diff 正常展示。
func TestConsoleDiffShownByDefault(t *testing.T) {
	var buf bytes.Buffer
	c := NewConsole(&buf, false, 2)

	c.PlayStart("p", []string{"h1"})
	c.TaskStart("配置", "template")
	c.HostResult("h1", &model.TaskResult{
		Host: "h1", Changed: true,
		Diff: "+port=8080\n",
	})
	c.TaskDone()

	if !strings.Contains(buf.String(), "port=8080") {
		t.Fatalf("普通任务 diff 应展示:\n%s", buf.String())
	}
}

// TestConsoleTaskTable 任务结果表格化：表头 + 对齐的状态列，正常主机在
// 聚合模式（level 0）不出现、-v 全量出现；多行详情在表格下方按主机分组。
func TestConsoleTaskTable(t *testing.T) {
	var buf bytes.Buffer
	c := NewConsole(&buf, false, 1)

	c.PlayStart("p", []string{"h1", "h2"})
	c.TaskStart("部署", "shell")
	c.HostResult("h1", &model.TaskResult{Host: "h1", Changed: true, Msg: "完成"})
	c.HostResult("h2", &model.TaskResult{Host: "h2", Failed: true, Msg: "返回 1"})
	c.TaskDone()

	out := buf.String()
	for _, want := range []string{"HOST", "STATUS", "DETAIL", "changed", "fatal", "完成", "返回 1", "ok=0 changed=1 failed=1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("任务表格缺少 %q:\n%s", want, out)
		}
	}
	// 状态列对齐：两行 status 文本起点一致（changed 与 fatal 同列）
	var chg, ft = -1, -1
	for l := range strings.SplitSeq(out, "\n") {
		if strings.Contains(l, "=") {
			continue // 汇总行
		}
		if i := strings.Index(l, " changed"); i >= 0 {
			chg = i + 1
		}
		if i := strings.Index(l, "  fatal"); i >= 0 {
			ft = i + 2
		}
	}
	if chg < 0 || ft < 0 || chg != ft {
		t.Fatalf("status 列未对齐 (%d vs %d):\n%s", chg, ft, out)
	}
}

// TestConsoleAggregateOmitsNormalHosts 聚合模式（缺省级别）仅呈现异常主机。
func TestConsoleAggregateOmitsNormalHosts(t *testing.T) {
	var buf bytes.Buffer
	c := NewConsole(&buf, false, 0)

	c.TaskStart("t", "shell")
	c.HostResult("h1", &model.TaskResult{Host: "h1", Msg: "正常输出"})
	c.HostResult("h2", &model.TaskResult{Host: "h2", Failed: true, Msg: "超时"})
	c.TaskDone()

	out := buf.String()
	if strings.Contains(out, "正常输出") {
		t.Fatalf("聚合模式不应显示正常主机:\n%s", out)
	}
	if !strings.Contains(out, "fatal") || !strings.Contains(out, "超时") {
		t.Fatalf("聚合模式应显示异常主机:\n%s", out)
	}
	if !strings.Contains(out, "HOST") {
		t.Fatalf("异常主机也应以表格呈现:\n%s", out)
	}
}

// TestConsoleMultilineDetailBlock 多行详情不进单元格，表格下方按主机分组，
// 且首行摘要仍在表格内。
func TestConsoleMultilineDetailBlock(t *testing.T) {
	var buf bytes.Buffer
	c := NewConsole(&buf, false, 2)

	c.TaskStart("t", "shell")
	c.HostResult("h1", &model.TaskResult{Host: "h1", Stdout: "line1\nline2\nline3\n"})
	c.TaskDone()

	out := buf.String()
	if !strings.Contains(out, "line1") || !strings.Contains(out, "line2") || !strings.Contains(out, "line3") {
		t.Fatalf("多行输出应完整保留:\n%s", out)
	}
	if !strings.Contains(out, "  h1:") {
		t.Fatalf("详情块缺少主机分组头:\n%s", out)
	}
}

// TestConsoleRecapTable RECAP 表格化：数字列右对齐，quiet 保持行式。
func TestConsoleRecapTable(t *testing.T) {
	var buf bytes.Buffer
	c := NewConsole(&buf, false, 0)

	stats := map[string]*model.Stats{
		"h1": {Ok: 2, Changed: 10},
		"h2": {Ok: 100, Failed: 1},
	}
	c.Recap("p", stats)

	out := buf.String()
	for _, want := range []string{"HOST", "CHANGED", "UNREACHABLE", "h1", "h2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("RECAP 表格缺少 %q:\n%s", want, out)
		}
	}
	// 右对齐：OK 列的 "2" 与 "100" 结束列位置一致
	var l1, l2 string
	for l := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(l, "h1") {
			l1 = l
		}
		if strings.HasPrefix(l, "h2") {
			l2 = l
		}
	}
	if l1 == "" || l2 == "" {
		t.Fatalf("缺少主机行:\n%s", out)
	}
	if strings.Index(l1, "2")+1 != strings.Index(l2, "100")+3 {
		t.Fatalf("数字列未右对齐:\n%s", out)
	}

	// quiet 行式 RECAP
	buf.Reset()
	q := NewConsole(&buf, false, -1)
	q.Recap("p", stats)
	legacy := buf.String()
	if !strings.Contains(legacy, "h1:") || !strings.Contains(legacy, "ok=2 changed=10") {
		t.Fatalf("quiet RECAP 应保持行式:\n%s", legacy)
	}
}

// TestConsoleQuietLegacyLines quiet 模式任务结果保持行式（脚本/管道友好）。
func TestConsoleQuietLegacyLines(t *testing.T) {
	var buf bytes.Buffer
	c := NewConsole(&buf, false, -1)

	c.TaskStart("t", "shell")
	c.HostResult("h1", &model.TaskResult{Host: "h1", Msg: "正常"})
	c.HostResult("h2", &model.TaskResult{Host: "h2", Failed: true, Msg: "超时"})
	c.TaskDone()

	out := buf.String()
	if !strings.Contains(out, "fatal: [h2]: 超时") {
		t.Fatalf("quiet 应保持行式异常输出:\n%s", out)
	}
	if strings.Contains(out, "正常") || strings.Contains(out, "STATUS") {
		t.Fatalf("quiet 不应输出正常主机或表格:\n%s", out)
	}
}

// TestConsolePerHostSummary 缺省聚合模式计数行下列出每主机状态（异常主机
// 附错误信息首行）；-v 表格已逐主机呈现不重复；超过 20 台退化为纯计数。
func TestConsolePerHostSummary(t *testing.T) {
	var buf bytes.Buffer
	c := NewConsole(&buf, false, 0)

	c.TaskStart("部署", "shell")
	c.HostResult("h1", &model.TaskResult{Host: "h1", Changed: true})
	c.HostResult("h2", &model.TaskResult{Host: "h2", Failed: true, Msg: "Non-zero return code"})
	c.HostResult("h3", &model.TaskResult{Host: "h3"})
	c.TaskDone()

	out := buf.String()
	for _, want := range []string{
		fmt.Sprintf("    %-20s changed", "h1"),
		fmt.Sprintf("    %-20s fatal: Non-zero return code", "h2"),
		fmt.Sprintf("    %-20s ok", "h3"),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("主机清单缺少 %q:\n%s", want, out)
		}
	}

	// Msg 为空时回退 stderr 首行
	buf.Reset()
	c.TaskStart("t", "shell")
	c.HostResult("h1", &model.TaskResult{Host: "h1", Failed: true, Stderr: "line1\nline2\n"})
	c.TaskDone()
	if out := buf.String(); !strings.Contains(out, "fatal: line1") {
		t.Fatalf("应回退 stderr 首行:\n%s", out)
	}

	// -v：表格已逐主机呈现，计数行下不再重复清单
	buf.Reset()
	v := NewConsole(&buf, false, 1)
	v.TaskStart("t", "shell")
	v.HostResult("h1", &model.TaskResult{Host: "h1", Changed: true})
	v.TaskDone()
	if out := buf.String(); strings.Contains(out, "\n    h1") {
		t.Fatalf("-v 不应有缩进主机清单行（表格已逐主机呈现）:\n%s", out)
	}

	// 超过 20 台：退化为纯计数，不输出主机清单
	buf.Reset()
	big := NewConsole(&buf, false, 0)
	big.TaskStart("t", "shell")
	for i := 1; i <= 21; i++ {
		big.HostResult(fmt.Sprintf("h%02d", i), &model.TaskResult{Host: fmt.Sprintf("h%02d", i), Changed: true})
	}
	big.TaskDone()
	out = buf.String()
	if !strings.Contains(out, "changed=21") {
		t.Fatalf("纯计数汇总缺失:\n%s", out)
	}
	if strings.Contains(out, fmt.Sprintf("%-20s changed", "h01")) {
		t.Fatalf("超阈值不应输出主机清单:\n%s", out)
	}
}

// TestConsoleLoopItemRows 聚合模式下 loop 异常项以独立行入表。
func TestConsoleLoopItemRows(t *testing.T) {
	var buf bytes.Buffer
	c := NewConsole(&buf, false, 0)

	c.TaskStart("t", "shell")
	c.HostResult("h1", &model.TaskResult{
		Host: "h1",
		Items: []*model.TaskResult{
			{Item: "a", Msg: "ok-item"},
			{Item: "b", Failed: true, Msg: "失败项"},
		},
	})
	c.TaskDone()

	out := buf.String()
	if !strings.Contains(out, "item=b 失败项") {
		t.Fatalf("loop 异常项应入表:\n%s", out)
	}
	if strings.Contains(out, "ok-item") {
		t.Fatalf("正常项不应显示:\n%s", out)
	}
}

// TestConsoleDebugVisibleAtAggregate debug 模块结果在聚合(0)与逐主机(1)
// 级别都展示（Msg 即产出物，status/巡检场景依赖）；quiet(-1) 仍仅异常。
func TestConsoleDebugVisibleAtAggregate(t *testing.T) {
	var buf bytes.Buffer
	c := NewConsole(&buf, false, 0)

	c.PlayStart("p", []string{"h1"})
	c.TaskStart("汇报", "debug")
	c.HostResult("h1", &model.TaskResult{Host: "h1", Module: "debug", Msg: "port=18080"})
	c.TaskDone()
	if out := buf.String(); !strings.Contains(out, "port=18080") {
		t.Fatalf("聚合模式下 debug 产出应可见:\n%s", out)
	}

	// 对照：普通模块 ok 结果聚合模式仍隐藏
	buf.Reset()
	c.PlayStart("p", []string{"h1"})
	c.TaskStart("探测", "shell")
	c.HostResult("h1", &model.TaskResult{Host: "h1", Module: "shell", Msg: "probe-done"})
	c.TaskDone()
	if out := buf.String(); strings.Contains(out, "probe-done") {
		t.Fatalf("普通模块 ok 结果在聚合模式不应展示:\n%s", out)
	}
}

// TestConsoleSingleLineDetailNoDup 长单行消息只出现一次：单元格不重复展示
// 截断文本，全量内容进详情块（回归：曾出现"截断行 + 完整块"双份输出）。
func TestConsoleSingleLineDetailNoDup(t *testing.T) {
	long := strings.Repeat("x", 120)
	var buf bytes.Buffer
	c := NewConsole(&buf, false, 0)

	c.PlayStart("p", []string{"h1"})
	c.TaskStart("汇报", "debug")
	c.HostResult("h1", &model.TaskResult{Host: "h1", Module: "debug", Msg: long})
	c.TaskDone()

	out := buf.String()
	if n := strings.Count(out, long); n != 1 {
		t.Fatalf("全量内容应恰好出现一次: count=%d\n%s", n, out)
	}
	runX := strings.Repeat("x", 20)
	n := 0
	for l := range strings.SplitSeq(out, "\n") {
		if strings.Contains(l, runX) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("含长内容的行应只有详情块一行（单元格不重复展示截断文本）: %d\n%s", n, out)
	}
}
