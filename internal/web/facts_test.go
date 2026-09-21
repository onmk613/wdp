package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"wdp/internal/store"
)

// TestHostFacts 主机详情：404 / 未登录拒绝 / 离线主机返回探活结果与
// 错误说明（在线路径由 e2e 覆盖，CI 无 agent）。
func TestHostFacts(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	if rec := do(t, h, "GET", "/api/hosts/9/facts", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/hosts/9/facts", nil, &token); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在主机应 404: %d", rec.Code)
	}

	id, err := st.CreateHost(&store.Host{Name: "facts-host", Address: "127.0.0.1", AgentPort: 17601})
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, "GET", "/api/hosts/"+fmt.Sprint(id)+"/facts", nil, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"probe"`) || !strings.Contains(body, "不可达") {
		t.Fatalf("离线主机应带 probe 与错误说明: %s", body)
	}
}

// TestHostFactsOnline 在线路径：mTLS loopback agent 上跑 setup 模块，
// facts 齐全（hostname/cpus/os.family 至少存在；macOS 演练环境也会输出
// unknown 家族而非报错）。
func TestHostFactsOnline(t *testing.T) {
	s, _ := newAppServerMTLS(t, 18764)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	rec := do(t, h, "GET", "/api/hosts/1/facts", nil, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200: %d %s", rec.Code, rec.Body)
	}
	var resp HostFactsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Probe.Status != "online" || resp.Error != "" || resp.Facts == nil {
		t.Fatalf("在线主机应采到 facts: %s", rec.Body)
	}
	if resp.Facts["hostname"] == "" || resp.Facts["os"] == nil {
		t.Fatalf("facts 关键字段缺失: %s", rec.Body)
	}
}
