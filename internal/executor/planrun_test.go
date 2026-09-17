package executor

// RunPlan 的回归测试：plan 执行路径必须还原编译期的逐 play 主机范围与
// (play, 主机) 维度的冻结变量域。
//
// 历史缺陷：playsOf 把每个重建 play 的 Hosts 写成 "all"，而合成 inventory
// 是全 plan 主机的并集——多 play chart（playA→组 a、playB→组 b）的第二个
// play 会作用到全部主机（安装任务下发到错误的节点）；冻结变量域以主机名
// 为键，同一主机出现在多个 play 时后一个 play 的 play vars 覆盖前一个。

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"wdp/internal/plan"
)

// planChartFiles 是最小可加载 chart（RunPlan 会物化后走 chart.Load）。
func planChartFiles() map[string]string {
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	return map[string]string{
		"chart.yaml":  enc("name: two\nversion: 0.1.0\nno_marker: true\n"),
		"deploy.yaml": enc("- hosts: a\n  tasks: [{debug: {msg: x}}]\n- hosts: b\n  tasks: [{debug: {msg: y}}]\n"),
	}
}

// planTask 构造一个 debug 任务（渲染 {{ .scope_tag }}）。
func planTask(idx int) *plan.ResolvedTask {
	return &plan.ResolvedTask{Idx: idx, Module: "debug", Args: map[string]any{"msg": "{{ .scope_tag }}"}}
}

// planHost 构造一个主机分片。
func planHost(playIdx int, host, playName, tag string) *plan.HostPlan {
	return &plan.HostPlan{
		PlayIdx: playIdx,
		Host:    host,
		Conn:    plan.HostConn{Conn: "fake"},
		Play:    plan.PlayMeta{Name: playName},
		Tasks:   []*plan.ResolvedTask{planTask(1)},
		Vars: map[string]any{
			"inventory_hostname": host,
			"scope_tag":          tag,
			"play_hosts":         []string{host},
		},
	}
}

// twoPlayPlan 构造两个 play、各一台主机的计划。
func twoPlayPlan(t *testing.T) *plan.Plan {
	t.Helper()
	p := &plan.Plan{
		SchemaVer: plan.SchemaVer,
		Chart:     "two",
		Version:   "0.1.0",
		Phase:     "deploy",
		Values:    map[string]any{},
		Files:     planChartFiles(),
		Hosts: []*plan.HostPlan{
			planHost(0, "h1", "playA", "play0"),
			planHost(1, "h2", "playB", "play1"),
		},
	}
	p.FillID()
	return p
}

// TestRunPlanPlayHostScoping 每个 play 只作用于编译期选中的主机。
func TestRunPlanPlayHostScoping(t *testing.T) {
	ex, rep := setup(t, okExec)
	if ex.RunPlan(context.Background(), twoPlayPlan(t)) {
		t.Fatalf("RunPlan 不应报告失败主机\n--- 实际事件 ---\n%s", rep.joined())
	}
	got := rep.joined()
	for _, want := range []string{
		"PLAY playA hosts=[h1]",
		"PLAY playB hosts=[h2]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺少 %q\n--- 实际事件 ---\n%s", want, got)
		}
	}
	// 反向断言：旧缺陷把每个 play 展开到全 plan 主机
	if strings.Contains(got, "hosts=[h1 h2]") {
		t.Fatalf("play 主机范围泄漏到全 plan 主机\n--- 实际事件 ---\n%s", got)
	}
}

// TestRunPlanScopeKeyedVars 同一主机出现在多个 play 时，冻结变量域按
// (play, 主机) 定位：h1 在 playA 看到 play0、在 playB 看到 play1。旧实现
// 以主机名为键，同一主机多个 play 的冻结域互相覆盖（本测试会失败）。
func TestRunPlanScopeKeyedVars(t *testing.T) {
	ex, rep := setup(t, okExec)
	p := twoPlayPlan(t)
	// h1 同时属于 playB（不同的 play vars 快照）
	p.Hosts = append(p.Hosts, planHost(1, "h1", "playB", "play1"))
	p.FillID()

	if ex.RunPlan(context.Background(), p) {
		t.Fatalf("RunPlan 不应报告失败主机\n--- 实际事件 ---\n%s", rep.joined())
	}
	got := rep.joined()
	for _, want := range []string{"msg=play0", "msg=play1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺少 %q（说明冻结变量域按主机名互相覆盖）\n--- 实际事件 ---\n%s", want, got)
		}
	}
}
