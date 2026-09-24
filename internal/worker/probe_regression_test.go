package worker

// 通道选择回归：已纳管主机（控制台传 mTLS 客户端）失败即离线，
// 不得回落到明文。回落是中间人可主动触发的降级：阻断 TLS 握手后
// 自己以明文 HTTP 应答，控制台就会把脚本、become 密码、制品与
// agent 二进制全部明文交出去。

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
