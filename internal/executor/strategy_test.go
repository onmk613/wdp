package executor

import (
	"context"
	"strings"
	"testing"

	"wdp/internal/connection"
	"wdp/internal/inventory"
	"wdp/internal/model"
)

// parseTestInv 解析内置测试 inventory。
func parseTestInv(t *testing.T) *inventory.Inventory {
	t.Helper()
	inv, err := inventory.Parse([]byte(testInv))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// gateTask 构造健康门任务（rc==0 判据，单次尝试不重试）。
func gateTask(script string) *model.Task {
	return &model.Task{
		Name: "health-gate", Module: "shell", FreeForm: script,
		Until: `{{ if eq .result.rc 0 }}ok{{ end }}`, Retries: 1,
	}
}

// setupStrategy 构造带策略的执行器；ExecFn 区分校验探测/健康门/回滚脚本。
func setupStrategy(t *testing.T, gateRC int) (*Executor, *captureReporter) {
	t.Helper()
	fakeMu.Lock()
	fakes = nil
	fakeMu.Unlock()
	connection.RegisterFactory("fake", func(h *model.Host, dc *connection.Defaults) (connection.Connection, error) {
		f := connection.NewFake(h)
		f.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
			s := req.Script
			switch {
			case strings.Contains(s, "sha256sum"):
				return connection.ExecResult{Code: 3}, nil // 文件不存在
			case strings.Contains(s, "health-probe"):
				return connection.ExecResult{Code: gateRC}, nil
			default:
				return connection.ExecResult{Code: 0}, nil
			}
		}
		fakeMu.Lock()
		fakes = append(fakes, f)
		fakeMu.Unlock()
		return f, nil
	})
	rep := &captureReporter{}
	return New(parseTestInv(t), connection.NewManager(), rep, Options{Forks: 2}), rep
}

// TestCanaryGateFailureRollback：金丝雀批次健康门失败 → 回滚该批 → 后续批次不执行。
func TestCanaryGateFailureRollback(t *testing.T) {
	ex, rep := setupStrategy(t, 1) // 健康门恒失败
	plays := []*model.Play{{
		Hosts: "webservers",
		Strategy: &model.Strategy{
			Type: "canary", Batch: "1",
			Gate: gateTask("health-probe"), AutoRollback: true,
		},
		Tasks: []*model.Task{
			{Name: "部署配置", Module: "copy",
				Args: map[string]any{"content": "v1", "dest": "/deploy/app.conf"}},
		},
	}}
	if !ex.Run(context.Background(), plays) {
		t.Fatalf("健康门失败应判定 play 失败:\n%s", rep.joined())
	}

	fakes := allFakes()
	// 金丝雀主机（h1）执行了 copy：内存文件表有落盘
	if got, ok := fakes[0].File("/deploy/app.conf"); !ok || got != "v1" {
		t.Fatalf("金丝雀 copy 未执行: %#v", fakes[0].Files)
	}
	// 回滚脚本已下发（新建文件 → rm）
	scripts := joinExecScripts(fakes)
	if !strings.Contains(scripts, "rm -rf -- '/deploy/app.conf'") {
		t.Fatalf("缺少回滚脚本:\n%s", scripts)
	}
	// 第二台主机未进入执行（批次终止 → 连接从未建立）
	if len(fakes) != 1 {
		t.Fatalf("canary 失败后 h2 不应建连，实际 %d 台", len(fakes))
	}
	if !strings.Contains(rep.joined(), "健康门未通过") {
		t.Fatalf("缺少终止消息:\n%s", rep.joined())
	}
}

// TestRollingGatePass：健康门通过 → 全部批次执行完且无回滚。
func TestRollingGatePass(t *testing.T) {
	ex, rep := setupStrategy(t, 0) // 健康门通过
	plays := []*model.Play{{
		Hosts: "webservers",
		Strategy: &model.Strategy{
			Type: "rolling", Batch: "1",
			Gate: gateTask("health-probe"), AutoRollback: true,
		},
		Tasks: []*model.Task{
			{Name: "部署配置", Module: "copy",
				Args: map[string]any{"content": "v1", "dest": "/deploy/app.conf"}},
		},
	}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	fakes := allFakes()
	for i, f := range fakes {
		if _, ok := f.File("/deploy/app.conf"); !ok {
			t.Fatalf("主机 %d 未部署", i+1)
		}
	}
	// 门通过不回滚部署文件（快照目录清理的 rm -rf 不算回滚）
	if strings.Contains(joinExecScripts(fakes), "rm -rf -- '/deploy/app.conf'") {
		t.Fatal("门通过时不应回滚")
	}
}

// TestBatchFailureRollback：批次任务自身失败 → 回滚并终止。
func TestBatchFailureRollback(t *testing.T) {
	fakeMu.Lock()
	fakes = nil
	fakeMu.Unlock()
	connection.RegisterFactory("fake", func(h *model.Host, dc *connection.Defaults) (connection.Connection, error) {
		f := connection.NewFake(h)
		f.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
			s := req.Script
			switch {
			case strings.Contains(s, "sha256sum"):
				return connection.ExecResult{Code: 3}, nil
			case strings.Contains(s, "will-fail"):
				return connection.ExecResult{Code: 1, Stderr: "boom"}, nil
			default:
				return connection.ExecResult{Code: 0}, nil
			}
		}
		fakeMu.Lock()
		fakes = append(fakes, f)
		fakeMu.Unlock()
		return f, nil
	})
	rep := &captureReporter{}
	ex := New(parseTestInv(t), connection.NewManager(), rep, Options{Forks: 2})
	plays := []*model.Play{{
		Hosts:    "h1",
		Strategy: &model.Strategy{Type: "rolling", Batch: "1", AutoRollback: true},
		Tasks: []*model.Task{
			{Name: "先写文件", Module: "copy",
				Args: map[string]any{"content": "x", "dest": "/deploy/a.conf"}},
			{Name: "失败任务", Module: "shell", FreeForm: "will-fail"},
		},
	}}
	if !ex.Run(context.Background(), plays) {
		t.Fatal("批次失败应判定失败")
	}
	scripts := joinExecScripts(allFakes())
	if !strings.Contains(scripts, "rm -rf -- '/deploy/a.conf'") {
		t.Fatalf("批次失败应回滚已写文件:\n%s", scripts)
	}
}

// TestParseBatchSize 覆盖百分比/绝对数/边界。
func TestParseBatchSize(t *testing.T) {
	cases := []struct {
		batch string
		total int
		want  int
	}{
		{"10%", 100, 10},
		{"10%", 15, 2}, // ceil(1.5)
		{"1%", 5, 1},   // min 1
		{"3", 100, 3},
		{"500", 10, 10}, // 封顶
		{"", 100, 25},   // 默认 25%
		{"", 3, 3},      // 少量主机一批
		{"x", 10, 3},    // 非法回退 25%
	}
	for _, c := range cases {
		if got := parseBatchSize(c.batch, c.total); got != c.want {
			t.Fatalf("parseBatchSize(%q,%d)=%d want %d", c.batch, c.total, got, c.want)
		}
	}
}

func joinExecScripts(fakes []*connection.Fake) string {
	var sb strings.Builder
	for _, f := range fakes {
		for _, r := range f.ExecLog {
			sb.WriteString(r.Script)
			sb.WriteString("\n---\n")
		}
	}
	return sb.String()
}
