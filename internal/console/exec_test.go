package console

// ExecOnHosts 的回归：timeoutSec<=0 必须在入口钳制——WithTimeout(ctx, 0)
// 立即到期会让每台主机必失败（表现为"全部主机瞬间 unreachable"）。

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/agent"
	"wdp/internal/chart"
	"wdp/internal/model"
	"wdp/internal/store"
)

// startAgentForTest 起回环 agent（无认证回环模式，测试装配）。
func startAgentForTest(t *testing.T) string {
	t.Helper()
	s := agent.New("")
	s.SetRunsDir(filepath.Join(t.TempDir(), "runs"))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	base := "http://" + ln.Addr().String()
	t.Cleanup(func() {
		resp, err := http.Post(base+"/shutdown", "", nil)
		if err == nil {
			resp.Body.Close()
		}
	})
	return base
}

// TestExecOnHostsClampsZeroTimeout timeoutSec=0 应取默认 120s 而非立即
// 到期：对回环 agent 执行 echo，钳制修复后应成功返回。
func TestExecOnHostsClampsZeroTimeout(t *testing.T) {
	base := startAgentForTest(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := &ExecService{
		Store: st,
		HostModel: func(ctx context.Context, h *store.Host) *model.Host {
			return &model.Host{Name: h.Name, Address: "127.0.0.1", Conn: "agent", AgentURL: base}
		},
	}
	res := svc.ExecOnHosts(context.Background(),
		[]*store.Host{{ID: 1, Name: "self"}}, "echo clamp-ok", 0, 0, nil)
	if len(res) != 1 {
		t.Fatalf("应有一台结果: %+v", res)
	}
	if res[0].Err != "" {
		t.Fatalf("钳制后 timeoutSec=0 应正常执行: %+v", res[0])
	}
	if res[0].Stdout == "" {
		t.Fatalf("应有 stdout: %+v", res[0])
	}
}

// TestMarkerValuesConcurrentAggregation 并发读取 marker 的聚合回归：
// 逐主机并发（信号量限流）但错误清单按 hosts 声明序汇总——并发化不得
// 引入结果漂移（此前串行最坏 N×30s）。become root 依赖目标机 sudo，测试
// 环境不可用，故用必然连接失败的死端口构造 failed 类结果验证聚合口径。
func TestMarkerValuesConcurrentAggregation(t *testing.T) {
	markerDir := t.TempDir()
	chartDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(chartDir, "chart.yaml"),
		[]byte("name: mv\nversion: 0.1.0\nmarker_dir: "+markerDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chartDir, "deploy.yaml"),
		[]byte("- hosts: all\n  tasks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ch, err := chart.Load(chartDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	// 12 台全部指向死端口：并发执行后 failed 清单应完整且与 hosts 声明序
	// 一致（串行口径），并发不丢不错
	n := 12
	hosts := make([]*model.Host, n)
	for i := range hosts {
		hosts[i] = &model.Host{Name: fmt.Sprintf("h%02d", i), Address: "127.0.0.1",
			Conn: "agent", AgentURL: "http://127.0.0.1:1"}
	}
	_, err = (&RunService{}).MarkerValues(context.Background(), ch, hosts, false)
	if err == nil {
		t.Fatal("全部不可达应报错")
	}
	msg := err.Error()
	last := -1
	for i := range hosts {
		idx := strings.Index(msg, fmt.Sprintf("h%02d (", i))
		if idx < 0 {
			t.Fatalf("主机 h%02d 缺失于错误清单: %s", i, msg)
		}
		if idx < last {
			t.Fatalf("错误清单应按 hosts 声明序: %s", msg)
		}
		last = idx
	}
}
