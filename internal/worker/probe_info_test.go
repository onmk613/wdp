package worker

// 探活顺带抓取 agent /info 的回归：在线时携带模块集；/info 缺失（老
// agent）不影响探活结论，模块集为 nil（未知，保留库内最后已知）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"wdp/internal/store"
)

// splitTestServer 把 httptest 服务地址拆成 probeOnce 需要的 Address/AgentPort。
func splitTestServer(ts *httptest.Server) (string, int) {
	u, err := url.Parse(ts.URL)
	if err != nil {
		panic(err)
	}
	host := u.Hostname()
	port, _ := strconv.Atoi(u.Port())
	return host, port
}

// TestProbeFetchesModules /health + /info 都在：结果带模块集。
func TestProbeFetchesModules(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true,"hostname":"h1","build":"1.0.0-x"}`))
		case "/info":
			_, _ = w.Write([]byte(`{"build":"1.0.0-x","proto":1,"modules":["file","shell"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	h := &store.Host{}
	h.Address, h.AgentPort = splitTestServer(ts)

	res := probeOnce(context.Background(), h, "http", probeClient)
	if res.Status != "online" {
		t.Fatalf("应在线: %+v", res)
	}
	if len(res.Modules) != 2 || res.Modules[0] != "file" || res.Modules[1] != "shell" {
		t.Fatalf("应带回 /info 模块集: %v", res.Modules)
	}
}

// TestProbeInfoOptional /info 404（老 agent）：在线结论不受影响、模块集 nil。
func TestProbeInfoOptional(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"hostname":"h1","build":"0.9.0"}`))
	}))
	defer ts.Close()

	h := &store.Host{}
	h.Address, h.AgentPort = splitTestServer(ts)
	res := probeOnce(context.Background(), h, "http", probeClient)
	if res.Status != "online" {
		t.Fatalf("老 agent 缺 /info 仍应在线: %+v", res)
	}
	if res.Modules != nil {
		t.Fatalf("缺 /info 时模块集应为 nil（未知，非空集）: %v", res.Modules)
	}
}
