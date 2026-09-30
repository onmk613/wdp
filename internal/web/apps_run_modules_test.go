package web

// 执行受理期的模块能力对账（分层方案 P2）：agent 上报的模块集缺了
// 版本清单里的模块 → 受理期 400；无上报（老 agent）与无清单（旧行）
// 放行——不阻塞存量。

import (
	"net/http"
	"strings"
	"testing"

	"wdp/internal/store"
)

// TestRunAppsModuleCheck 单元口径：缺模块报错聚合、未知放行。
func TestRunAppsModuleCheck(t *testing.T) {
	s, st := newTestServer(t)

	appID, err := st.CreateApp("mc-app", "", "{}", nil, nil, "1.0.0", "/tmp/mc.tgz", "sha", 1,
		[]string{"deploy"}, `{"deploy":["shell","copy"],"uninstall":["file"]}`)
	if err != nil {
		t.Fatal(err)
	}
	items := []runItem{{app: &store.App{ID: appID}, version: "1.0.0", tgz: "/tmp/mc.tgz", phase: "deploy"}}

	mkHost := func(name, modules string) *store.Host {
		id, err := st.CreateHost(&store.Host{Name: name, Address: "127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		if modules != "" {
			if err := st.SetHostStatus(id, "online", "9.9.9-x", modules); err != nil {
				t.Fatal(err)
			}
		}
		h, err := st.GetHost(id)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	// 1) 老 agent（无上报）：放行
	if err := s.runAppsModuleCheck([]*store.Host{mkHost("old", "")}, items); err != nil {
		t.Fatalf("无上报应放行: %v", err)
	}
	// 2) 模块齐全：放行
	if err := s.runAppsModuleCheck([]*store.Host{mkHost("full", `["shell","copy"]`)}, items); err != nil {
		t.Fatalf("模块齐全应放行: %v", err)
	}
	// 3) 缺 copy：报错指名主机与模块，且只报 deploy 相位（uninstall 的 file 不掺入）
	err = s.runAppsModuleCheck([]*store.Host{mkHost("lag", `["shell"]`)}, items)
	if err == nil {
		t.Fatal("缺模块应报错")
	}
	if !strings.Contains(err.Error(), "lag") || !strings.Contains(err.Error(), "copy") {
		t.Fatalf("报错应指名主机与缺失模块: %v", err)
	}
	if strings.Contains(err.Error(), "file") {
		t.Fatalf("未执行相位的模块不应掺入: %v", err)
	}

	// 4) 版本无清单（旧行）：放行
	oldID, err := st.CreateApp("mc-old", "", "{}", nil, nil, "1.0.0", "/tmp/mc-old.tgz", "sha", 1, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	oldItems := []runItem{{app: &store.App{ID: oldID}, version: "1.0.0", tgz: "/tmp/mc-old.tgz", phase: "deploy"}}
	if err := s.runAppsModuleCheck([]*store.Host{mkHost("lag2", `["shell"]`)}, oldItems); err != nil {
		t.Fatalf("无清单应放行: %v", err)
	}
}

// TestRunAppsModuleGateHTTP HTTP 口径：受理期 400（不建 run），报错可读。
func TestRunAppsModuleGateHTTP(t *testing.T) {
	s, st := newTestServer(t)
	token := loginSession(t, s)

	appID, err := st.CreateApp("gate-app", "", "{}", nil, nil, "1.0.0", "/tmp/gate.tgz", "sha", 1,
		[]string{"deploy"}, `{"deploy":["shell","template"]}`)
	if err != nil {
		t.Fatal(err)
	}
	hostID, err := st.CreateHost(&store.Host{Name: "gate-host", Address: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetHostStatus(hostID, "online", "0.9.0-old", `["shell"]`); err != nil {
		t.Fatal(err)
	}

	rec := do(t, s.Handler(), "POST", "/api/runs", map[string]any{
		"items":    []map[string]any{{"app_id": appID}},
		"selector": map[string]any{"kind": "all"},
	}, &token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺模块应 400: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "template") || !strings.Contains(rec.Body.String(), "gate-host") {
		t.Fatalf("报错应指名主机与缺失模块: %s", rec.Body)
	}
	// 不留 run 残行（受理期拒绝，非执行期失败）
	runs, err := st.ListRuns(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.AppName == "gate-app" {
			t.Fatalf("受理期拒绝不应建 run: %+v", r)
		}
	}
}
