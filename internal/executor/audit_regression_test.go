package executor

// 审计修复回归测试：always 失败传播 / block tags 过滤 / serial 空段报错。

import (
	"context"
	"strings"
	"testing"

	"wdp/internal/connection"
	"wdp/internal/model"
)

// TestBlockAlwaysFailureFailsPlay 回归：block 主体成功、always 清理任务失败时
// block 必须判失败（此前失败被吞、部署误报成功，RECAP 与退出码矛盾）。
func TestBlockAlwaysFailureFailsPlay(t *testing.T) {
	ex, rep := setup(t, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		if strings.Contains(req.Script, "always-step") {
			return connection.ExecResult{Code: 1, Stderr: "boom"}, nil
		}
		return connection.ExecResult{Code: 0, Stdout: "ok\n"}, nil
	})
	play := &model.Play{
		Name:  "always-fail",
		Hosts: "h1",
		Tasks: []*model.Task{
			{
				Name: "组",
				Block: []*model.Task{
					{Name: "主操作", Module: "shell", FreeForm: "deploy.sh"},
				},
				Always: []*model.Task{
					{Name: "清理", Module: "shell", FreeForm: "always-step"},
				},
			},
		},
	}
	if failed := ex.Run(context.Background(), []*model.Play{play}); !failed {
		t.Fatalf("always 任务失败应使 play 判失败:\n%s", rep.joined())
	}
	// locale 无关断言：block 结果 failed=true 且消息携带 always 任务名
	out := rep.joined()
	if !strings.Contains(out, "failed=true") || !strings.Contains(out, "清理") {
		t.Fatalf("失败消息应指明 always 任务:\n%s", out)
	}
}

// TestBlockAlwaysFailureAfterRescue rescue 成功兜底后 always 失败同样判失败。
func TestBlockAlwaysFailureAfterRescue(t *testing.T) {
	ex, rep := setup(t, func(host string, req connection.ExecRequest) (connection.ExecResult, error) {
		if strings.Contains(req.Script, "always-step") || strings.Contains(req.Script, "deploy.sh") {
			return connection.ExecResult{Code: 1, Stderr: "boom"}, nil
		}
		return connection.ExecResult{Code: 0, Stdout: "ok\n"}, nil
	})
	play := &model.Play{
		Name:  "always-after-rescue",
		Hosts: "h1",
		Tasks: []*model.Task{
			{
				Name: "组",
				Block: []*model.Task{
					{Name: "主操作", Module: "shell", FreeForm: "deploy.sh"},
				},
				Rescue: []*model.Task{
					{Name: "恢复", Module: "shell", FreeForm: "rescue.sh"},
				},
				Always: []*model.Task{
					{Name: "清理", Module: "shell", FreeForm: "always-step"},
				},
			},
		},
	}
	if failed := ex.Run(context.Background(), []*model.Play{play}); !failed {
		t.Fatalf("rescue 成功但 always 失败应判失败:\n%s", rep.joined())
	}
}

// TestBlockTagsFilter 回归：--tags 对 block 内部任务生效
// （此前 block 子任务绕过 tag 过滤，被 skip 的任务照样执行）。
// 覆盖三种语义：子任务 tag 命中选中整组、组内未命中子任务跳过、
// 容器 tag 继承到未打 tag 的子任务。
func TestBlockTagsFilter(t *testing.T) {
	newPlay := func() *model.Play {
		return &model.Play{
			Name:  "tags",
			Hosts: "h1",
			Tasks: []*model.Task{
				{
					Name: "组",
					Block: []*model.Task{
						{Name: "要跑的", Module: "shell", FreeForm: "true", Tags: []string{"wanted"}},
						{Name: "不该跑的", Module: "shell", FreeForm: "false", Tags: []string{"other"}},
					},
				},
			},
		}
	}

	// 子任务 tag 命中：整组选中，组内按 tag 逐子过滤
	ex, _ := setup(t, okExec)
	ex.Opts.Tags = []string{"wanted"}
	if failed := ex.Run(context.Background(), []*model.Play{newPlay()}); failed {
		t.Fatal("不应失败")
	}
	if n := execCount(); n != 1 {
		t.Fatalf("tag 过滤后 block 内应只执行 1 个任务，实际 %d", n)
	}

	// 容器 tag 继承：容器命中时未打 tag 的子任务同样执行
	ex2, _ := setup(t, okExec)
	ex2.Opts.Tags = []string{"grp"}
	play2 := &model.Play{
		Name:  "tags-inherit",
		Hosts: "h1",
		Tasks: []*model.Task{
			{
				Name: "组",
				Tags: []string{"grp"},
				Block: []*model.Task{
					{Name: "子任务", Module: "shell", FreeForm: "true"},
				},
			},
		},
	}
	if failed := ex2.Run(context.Background(), []*model.Play{play2}); failed {
		t.Fatal("不应失败")
	}
	if n := execCount(); n != 1 {
		t.Fatalf("容器 tag 应继承到子任务，实际执行 %d", n)
	}
}

func execCount() int {
	n := 0
	for _, f := range allFakes() {
		n += len(f.ExecLog)
	}
	return n
}

// TestSplitBatchesEmptySegment 回归：serial "5," 尾逗号报错
// （此前空段静默回退 25% 默认分批）。
func TestSplitBatchesEmptySegment(t *testing.T) {
	hosts := make([]*model.Host, 8)
	for i := range hosts {
		hosts[i] = &model.Host{Name: string(rune('a' + i))}
	}
	if _, err := splitBatches(hosts, "5,"); err == nil {
		t.Fatal("serial 含空段应报错")
	}
	if _, err := splitBatches(hosts, "5, ,3"); err == nil {
		t.Fatal("serial 含空白段应报错")
	}
	batches, err := splitBatches(hosts, "3,5")
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || len(batches[0]) != 3 || len(batches[1]) != 5 {
		t.Fatalf("正常 serial 分批错误: %v", batches)
	}
}
