package worker

// 通道选择回归：已纳管主机（控制台传 mTLS 客户端）失败即离线，
// 不得回落到明文。回落是中间人可主动触发的降级：阻断 TLS 握手后
// 自己以明文 HTTP 应答，控制台就会把脚本、become 密码、制品与
// agent 二进制全部明文交出去。

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wdp/internal/store"
)

// TestProbeHostNoPlaintextFallback 明文 agent 面对 mTLS 探测必须判离线，
// 而不是"https 失败就退回 http 当成成功"。
func TestProbeHostNoPlaintextFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"hostname":"plain"}`))
	}))
	defer srv.Close()

	h := hostFromURL(t, srv.URL)
	tlsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}

	// 传 mTLS 客户端（= 台账要求 TLS）：只能 https，明文应答不得被接受
	res := ProbeHost(context.Background(), h, tlsClient)
	if res.Status == "online" {
		t.Fatalf("要求 TLS 时不得接受明文应答（曾经的 https→http 回落）: %+v", res)
	}
	if !strings.Contains(res.Error, "明文通道") {
		t.Fatalf("失败信息应给出可操作出路: %q", res.Error)
	}

	// 不传客户端（= 台账显式声明明文）：走 http
	res2 := ProbeHost(context.Background(), h, nil)
	if res2.Status != "online" || res2.Scheme != "http" {
		t.Fatalf("显式明文主机应走 http: %+v", res2)
	}
}

// hostFromURL 从 httptest URL 抽出 address/port 组台账行。
func hostFromURL(t *testing.T, raw string) *store.Host {
	t.Helper()
	hostport := strings.TrimPrefix(raw, "http://")
	addr, portStr, ok := strings.Cut(hostport, ":")
	if !ok {
		t.Fatalf("无法解析测试地址: %s", raw)
	}
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	return &store.Host{Name: "probe-fixture", Address: addr, AgentPort: port}
}

// TestProberCancelledCtxKeepsStatus 关停瞬间伪 offline 回归：探测回调内
// 取消 ctx（模拟 server 关停与已派发探测的竞态——取消先于结论落库），
// 失败结论不得写回主机状态；对照组：ctx 存活时同一失败照常标离线。
func TestProberCancelledCtxKeepsStatus(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "wdp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.CreateHost(&store.Host{Name: "probe-ctx", Address: "127.0.0.1", AgentPort: 1})
	if err != nil {
		t.Fatal(err)
	}
	setStatus := func(status string) string {
		h, herr := st.GetHost(id)
		if herr != nil {
			t.Fatal(herr)
		}
		return h.Status
	}

	// 对照组：ctx 存活，探测失败 → 状态落 offline（Run 是阻塞循环，
	// 后台跑、轮询到状态写入后取消退出）
	ctxAlive, stopAlive := context.WithCancel(context.Background())
	p := &Prober{Store: st, Logger: slog.New(slog.DiscardHandler), Every: time.Hour,
		Probe: func(context.Context, *store.Host) ProbeResult {
			return ProbeResult{Status: "offline", Error: "connection refused"}
		}}
	done := make(chan struct{})
	go func() { p.Run(ctxAlive); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for setStatus("") != "offline" {
		if time.Now().After(deadline) {
			t.Fatal("对照组：失败探测应标 offline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopAlive()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 Run 应退出")
	}

	// 回归组：探测回调内先取消 ctx 再返回失败 → 不写状态
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p2 := &Prober{Store: st, Logger: slog.New(slog.DiscardHandler), Every: time.Hour,
		Probe: func(ctx context.Context, h *store.Host) ProbeResult {
			cancel() // 关停先于结论落库
			return ProbeResult{Status: "offline", Error: "context canceled"}
		}}
	if err := st.SetHostStatus(id, "online", "test-build", ""); err != nil { // 预置在线，观察是否被覆盖
		t.Fatal(err)
	}
	done2 := make(chan struct{})
	go func() { p2.Run(ctx); close(done2) }()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 Run 应退出")
	}
	if got := setStatus(""); got != "online" {
		t.Fatalf("ctx 取消后的失败结论不应覆盖主机状态，实际 %q", got)
	}
}
