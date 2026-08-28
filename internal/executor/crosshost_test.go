package executor

// P0/P2 跨主机能力回归：set_fact + hostvars（两段式编排）、
// fact cache 跨运行复用、add_host 运行期扩主机。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/conn"
	"wdp/internal/model"
)

// TestSetFactCrossHostVisible 两段式编排：play1 各主机 set_fact，
// play2 经 .hostvars 读到其他主机写入的事实。
func TestSetFactCrossHostVisible(t *testing.T) {
	ex, rep := setupFeature(t, false, func(host string, req conn.ExecRequest) (conn.ExecResult, error) {
		return conn.ExecResult{Code: 0, Stdout: "ran: " + req.Script + "\n"}, nil
	})
	plays := []*model.Play{
		{
			Name:  "collect",
			Hosts: "webservers",
			Tasks: []*model.Task{
				{Name: "登记节点号", Module: "set_fact", Args: map[string]any{
					"node_id": "{{ .inventory_hostname }}-id",
				}},
			},
		},
		{
			Name:  "configure",
			Hosts: "webservers",
			Tasks: []*model.Task{
				{Name: "引用同伴事实", Module: "shell",
					FreeForm: `peer={{ (index .hostvars "h2").node_id }} self={{ .node_id }}`},
			},
		},
	}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	scripts := joinExecScripts(allFakes())
	// 两台主机的 play2 都应读到 h2 的事实
	if strings.Count(scripts, "peer=h2-id self=h1-id")+strings.Count(scripts, "peer=h2-id self=h2-id") != 2 {
		t.Fatalf("hostvars 跨主机读取失败:\n%s\n报告:\n%s", scripts, rep.joined())
	}
}

// TestSetFactLocalScopeRegisterNotLeaked register 结果不进入 hostvars
// （边界：register 本机域，set_fact 事实库）。
func TestSetFactLocalScopeRegisterNotLeaked(t *testing.T) {
	ex, rep := setupFeature(t, false, func(host string, req conn.ExecRequest) (conn.ExecResult, error) {
		return conn.ExecResult{Code: 0, Stdout: "secret-from-h2"}, nil
	})
	plays := []*model.Play{
		{Name: "collect", Hosts: "webservers", Tasks: []*model.Task{
			{Name: "注册", Module: "shell", FreeForm: "anything", Register: "local_out"},
		}},
		{Name: "configure", Hosts: "webservers", Tasks: []*model.Task{
			{Name: "register 不跨主机", Module: "shell",
				FreeForm: `{{ if dig "local_out" "" (index .hostvars "h2") }}leaked{{ else }}clean{{ end }}`},
		}},
	}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	if strings.Contains(joinExecScripts(allFakes()), "leaked") {
		t.Fatalf("register 结果不应进入 hostvars:\n%s", joinExecScripts(allFakes()))
	}
}

// TestFactCachePersistence fact cache 跨运行：第一次运行写入 setup/set_fact，
// 第二次运行（全新执行器）加载后无需重新采集即可引用。
func TestFactCachePersistence(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "facts.json")

	runOnce := func() {
		ex, rep := setupFeature(t, false, func(host string, req conn.ExecRequest) (conn.ExecResult, error) {
			return conn.ExecResult{Code: 0, Stdout: "ran: " + req.Script + "\n"}, nil
		})
		ex.Opts.FactCachePath = cachePath
		ex.loadFactCache(cachePath) // 模拟 Options 在 New 后注入（同 New 行为）
		plays := []*model.Play{{Hosts: "h1", Tasks: []*model.Task{
			{Name: "写事实", Module: "set_fact", Args: map[string]any{"persisted": "yes"}},
		}}}
		if ex.Run(context.Background(), plays) {
			t.Fatalf("不应失败:\n%s", rep.joined())
		}
	}

	runOnce()
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("fact cache 未落盘: %v", err)
	}
	var cached map[string]map[string]any
	if err := json.Unmarshal(data, &cached); err != nil {
		t.Fatalf("fact cache 非法 JSON: %v", err)
	}
	if cached["h1"]["persisted"] != "yes" {
		t.Fatalf("fact cache 内容: %s", data)
	}

	// 第二次运行：新执行器，直接引用上一轮持久化的事实
	ex2, rep2 := setupFeature(t, false, func(host string, req conn.ExecRequest) (conn.ExecResult, error) {
		return conn.ExecResult{Code: 0, Stdout: "ran: " + req.Script + "\n"}, nil
	})
	ex2.Opts.FactCachePath = cachePath
	ex2.loadFactCache(cachePath)
	plays2 := []*model.Play{{Hosts: "h1", Tasks: []*model.Task{
		{Name: "读缓存事实", Module: "shell", FreeForm: "v={{ .persisted }}"},
	}}}
	if ex2.Run(context.Background(), plays2) {
		t.Fatalf("不应失败:\n%s", rep2.joined())
	}
	if !strings.Contains(joinExecScripts(allFakes()), "v=yes") {
		t.Fatalf("跨运行 fact 未生效:\n%s", joinExecScripts(allFakes()))
	}
}

// TestFactCacheCorruptIgnored cache 损坏不阻塞部署（告警后忽略）。
func TestFactCacheCorruptIgnored(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "facts.json")
	if err := os.WriteFile(cachePath, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	ex, rep := setupFeature(t, false, func(host string, req conn.ExecRequest) (conn.ExecResult, error) {
		return conn.ExecResult{Code: 0}, nil
	})
	ex.Opts.FactCachePath = cachePath
	ex.loadFactCache(cachePath)
	plays := []*model.Play{{Hosts: "h1", Tasks: []*model.Task{
		{Module: "shell", FreeForm: "true"},
	}}}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("损坏 cache 不应阻塞:\n%s", rep.joined())
	}
}

// TestAddHostRuntimeSelectable add_host 后续 play 可选中新主机，
// groups/hosts/hostvars 同步可见。
func TestAddHostRuntimeSelectable(t *testing.T) {
	ex, rep := setupFeature(t, false, func(host string, req conn.ExecRequest) (conn.ExecResult, error) {
		return conn.ExecResult{Code: 0, Stdout: "ran[" + host + "]: " + req.Script + "\n"}, nil
	})
	plays := []*model.Play{
		{
			Name:  "扩容",
			Hosts: "h1",
			Tasks: []*model.Task{
				{Name: "注册新节点", Module: "add_host", Args: map[string]any{
					"name": "node5", "address": "10.0.0.5", "conn": "fake",
					"groups": []any{"extra"},
					"vars":   map[string]any{"zone": "az2"},
				}},
			},
		},
		{
			Name:  "编排新节点",
			Hosts: "extra",
			Tasks: []*model.Task{
				{Name: "新节点上执行", Module: "shell",
					FreeForm: `zone={{ .zone }} in_group={{ index .groups "extra" }} addr={{ (index .hosts "node5").address }}`},
			},
		},
	}
	if ex.Run(context.Background(), plays) {
		t.Fatalf("不应失败:\n%s", rep.joined())
	}
	joined := rep.joined()
	if !strings.Contains(joined, "PLAY 编排新节点 hosts=[node5]") ||
		!strings.Contains(joined, "node5 新节点上执行") {
		t.Fatalf("新主机未被后续 play 选中:\n%s", joined)
	}
	scripts := joinExecScripts(allFakes())
	if !strings.Contains(scripts, "zone=az2") || !strings.Contains(scripts, "addr=10.0.0.5") {
		t.Fatalf("新主机变量/元信息未生效:\n%s", scripts)
	}
}
