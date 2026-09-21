package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"wdp/internal/model"
)

func TestJSONReporter(t *testing.T) {
	var buf bytes.Buffer
	r := NewJSONReporter(&buf)

	r.PlayStart("部署", []string{"h1", "h2"})
	r.TaskStart("执行", "shell")
	r.HostResult("h1", &model.TaskResult{Host: "h1", Changed: true, Msg: "ok"})
	r.HostResult("h2", &model.TaskResult{Host: "h2", Failed: true, Msg: "boom"})
	r.TaskDone()
	r.PlayMsg("进度消息不应进入 JSON")
	r.Recap("部署", map[string]*model.Stats{
		"h1": {Ok: 0, Changed: 1},
		"h2": {Failed: 1},
	})
	r.Finish()

	var doc struct {
		Plays []struct {
			Name  string   `json:"name"`
			Hosts []string `json:"hosts"`
			Tasks []struct {
				Name    string             `json:"name"`
				Results []model.TaskResult `json:"results"`
				Summary *model.Stats       `json:"summary"`
			} `json:"tasks"`
			Recap map[string]*model.Stats `json:"recap"`
		} `json:"plays"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("非法 JSON: %v\n%s", err, buf.String())
	}
	if len(doc.Plays) != 1 || doc.Plays[0].Name != "部署" {
		t.Fatalf("%s", buf.String())
	}
	task := doc.Plays[0].Tasks[0]
	if len(task.Results) != 2 {
		t.Fatalf("结果数 %d", len(task.Results))
	}
	if task.Summary.Changed != 1 || task.Summary.Failed != 1 {
		t.Fatalf("summary: %+v", task.Summary)
	}
	if doc.Plays[0].Recap["h2"].Failed != 1 {
		t.Fatalf("recap: %+v", doc.Plays[0].Recap)
	}
}

func TestJSONReporterEmptyFinish(t *testing.T) {
	var buf bytes.Buffer
	r := NewJSONReporter(&buf)
	r.Finish()
	if !bytes.Contains(buf.Bytes(), []byte(`"plays": []`)) && !bytes.Contains(buf.Bytes(), []byte(`"plays":[]`)) {
		t.Fatalf("空 run 应输出空 plays: %s", buf.String())
	}
}

// TestJSONReporterNoLogRedaction no_log 任务的 stdout/stderr/msg/diff
// 与 loop 逐项输出必须遮蔽，不能随 JSON 报告泄漏进 CI 工件。
func TestJSONReporterNoLogRedaction(t *testing.T) {
	var buf bytes.Buffer
	r := NewJSONReporter(&buf)

	r.PlayStart("部署", []string{"h1"})
	r.TaskStart("敏感任务", "shell")
	r.HostResult("h1", &model.TaskResult{
		Host: "h1", Changed: true, NoLog: true,
		Stdout: "SECRET-stdout", Stderr: "SECRET-stderr", Msg: "SECRET-msg",
		Items: []*model.TaskResult{
			{Host: "h1", Item: "k1", Stdout: "SECRET-item", Msg: "SECRET-item-msg"},
		},
	})
	r.TaskDone()
	r.Finish()

	out := buf.String()
	for _, leak := range []string{"SECRET-stdout", "SECRET-stderr", "SECRET-msg", "SECRET-item"} {
		if strings.Contains(out, leak) {
			t.Errorf("no_log 泄漏进 JSON: %s 出现于\n%s", leak, out)
		}
	}
	// json.Encoder 默认 HTML 转义（< → \u003c），断言匹配两种形态
	if !strings.Contains(out, "u003credacted") {
		t.Errorf("应包含遮蔽占位 <redacted>:\n%s", out)
	}
	if !strings.Contains(out, `"no_log": true`) {
		t.Errorf("应标记 no_log=true 供消费方识别:\n%s", out)
	}
}

// TestJSONReporterNoLogDoesNotMutateOriginal 遮蔽作用于副本，原结果（register 数据源）保持完整。
func TestJSONReporterNoLogDoesNotMutateOriginal(t *testing.T) {
	var buf bytes.Buffer
	r := NewJSONReporter(&buf)
	orig := &model.TaskResult{Host: "h1", NoLog: true, Stdout: "SECRET", Msg: "SECRET"}
	r.PlayStart("p", []string{"h1"})
	r.TaskStart("t", "shell")
	r.HostResult("h1", orig)
	r.Finish()
	if orig.Stdout != "SECRET" || orig.Msg != "SECRET" {
		t.Fatalf("原结果被改写: %+v", orig)
	}
}
