package executor

import (
	"context"
	"strings"
	"testing"

	"wdp/internal/conn"
	"wdp/internal/conn/fake"
	"wdp/internal/inventory"
	"wdp/internal/model"
)

// fail-fast：任一主机失败即中止后续批次与后续 play（在途主机会执行完）。
// 无 strategy 的传统模式默认批次失败不阻断（play.go 的 continue 语义），
// FailFast 覆盖它；strategy 配置自带批次中止，不在本测试范围。

const failFastInv = `
webservers:
  hosts:
    h1: {conn: fake}
    h2: {conn: fake}
    h3: {conn: fake}
`

// setupFailFast 构造三主机执行器：h1 的任务失败，其余成功。
func setupFailFast(t *testing.T, failFast bool) (*Executor, *captureReporter) {
	t.Helper()
	fakeMu.Lock()
	fakes = nil
	fakeMu.Unlock()
	conn.RegisterFactory("fake", func(h *model.Host, dc *conn.Defaults) (conn.Conn, error) {
		f := fake.NewFake(h)
		f.ExecFn = func(req conn.ExecRequest) (conn.ExecResult, error) {
			if h.Name == "h1" {
				return conn.ExecResult{Code: 1, Stderr: "boom"}, nil
			}
			return conn.ExecResult{Code: 0}, nil
		}
		fakeMu.Lock()
		fakes = append(fakes, f)
		fakeMu.Unlock()
		return f, nil
	})
	inv, err := inventory.Parse([]byte(failFastInv))
	if err != nil {
		t.Fatal(err)
	}
	rep := &captureReporter{}
	return New(inv, conn.NewManager(), rep, Options{Forks: 2, FailFast: failFast}), rep
}

// TestFailFastAbortsSubsequentBatches serial=1 三批：首批 h1 失败后，
// fail-fast 不再推进后续批次（h2/h3 从未连接）；关闭时传统语义全量执行。
func TestFailFastAbortsSubsequentBatches(t *testing.T) {
	play := &model.Play{
		Hosts: "webservers", Serial: "1",
		Tasks: []*model.Task{{Name: "t", Module: "shell", FreeForm: "check"}},
	}

	ex, rep := setupFailFast(t, true)
	if !ex.Run(context.Background(), []*model.Play{play}) {
		t.Fatal("存在失败主机，Run 应返回 true")
	}
	if n := len(allFakes()); n != 1 {
		t.Fatalf("fail-fast 应只执行首批（仅 h1 建立连接），实际 %d 台", n)
	}
	if !strings.Contains(rep.joined(), "fail-fast: batch had failed hosts") {
		t.Fatalf("应给出批次中止消息:\n%s", rep.joined())
	}

	ex2, _ := setupFailFast(t, false)
	if !ex2.Run(context.Background(), []*model.Play{play}) {
		t.Fatal("传统模式同样存在失败主机")
	}
	if n := len(allFakes()); n != 3 {
		t.Fatalf("传统语义应执行全部主机，实际 %d 台", n)
	}
}

// TestFailFastAbortsRemainingPlays 首个 play 失败后不再进入后续 play。
func TestFailFastAbortsRemainingPlays(t *testing.T) {
	mk := func(name string) *model.Play {
		return &model.Play{Hosts: "webservers", Tasks: []*model.Task{
			{Name: name, Module: "shell", FreeForm: "check"},
		}}
	}
	ex, rep := setupFailFast(t, true)
	if !ex.Run(context.Background(), []*model.Play{mk("p1"), mk("p2")}) {
		t.Fatal("存在失败主机，Run 应返回 true")
	}
	if !strings.Contains(rep.joined(), "aborting remaining 1 play(s)") {
		t.Fatalf("应中止后续 play 并提示:\n%s", rep.joined())
	}
	if strings.Contains(rep.joined(), "PLAY p2") {
		t.Fatalf("后续 play 不应执行:\n%s", rep.joined())
	}
}
