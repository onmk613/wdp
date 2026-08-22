package agentconn

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"wdp/internal/connection"
	"wdp/internal/model"
)

// TestNativeExtract 原生解压：新 agent 200 成功；无该端点的旧版 agent
// 返回 404 → ErrNativeUnsupported（模块据此回退 shell 路径）。
func TestNativeExtract(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /archive", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("src") == "" || r.URL.Query().Get("dest") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"files":3}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	if err := New(&model.Host{AgentURL: ts.URL}, nil).NativeExtract(context.Background(), "/a.tgz", "/opt/a"); err != nil {
		t.Fatalf("原生解压应成功: %v", err)
	}

	old := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(old.Close)
	err := New(&model.Host{AgentURL: old.URL}, nil).NativeExtract(context.Background(), "/a.tgz", "/opt/a")
	if !errors.Is(err, connection.ErrNativeUnsupported) {
		t.Fatalf("旧版 agent 应返回不支持哨兵: %v", err)
	}
}

// TestBaseURLAssembly 验证连接地址拼装：裸域名/IPv4/IPv6 拼接 agent 端口
// （IPv6 字面量自动加方括号），已含端口的 host:port 与 [ipv6]:port 原样保留。
func TestBaseURLAssembly(t *testing.T) {
	cases := []struct {
		name string
		host *model.Host
		want string
	}{
		{"裸域名拼默认端口", &model.Host{Address: "node1.example.com"}, "http://node1.example.com:7602"},
		{"裸IPv4拼默认端口", &model.Host{Address: "10.0.0.5"}, "http://10.0.0.5:7602"},
		{"裸IPv6拼默认端口", &model.Host{Address: "fd00::5"}, "http://[fd00::5]:7602"},
		{"IPv6自定义端口", &model.Host{Address: "fd00::5", AgentPort: 7700}, "http://[fd00::5]:7700"},
		{"IPv4含端口原样", &model.Host{Address: "10.0.0.5:9000"}, "http://10.0.0.5:9000"},
		{"IPv6含端口原样", &model.Host{Address: "[fd00::5]:9000"}, "http://[fd00::5]:9000"},
		{"TLS方案", &model.Host{Address: "10.0.0.5", TLS: true}, "https://10.0.0.5:7602"},
	}
	for _, tc := range cases {
		if got := New(tc.host, nil).base; got != tc.want {
			t.Errorf("%s: base=%q want %q", tc.name, got, tc.want)
		}
	}
}
