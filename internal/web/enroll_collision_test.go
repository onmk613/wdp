package web

// 主机名碰撞防护：未绑定主机名的 enroll token 不允许 claim 既有主机的
// 名字——否则"证书已存在即跳过"的幂等分支会把既有主机的 mTLS 私钥交付
// 给撞名者，done 还会改写台账地址。绑定主机名的 token 是管理员对该主机
// 的重装/迁移授权，不受此限。

import (
	"encoding/json"
	"net/http"
	"testing"

	"wdp/internal/store"
)

// TestEnrollNameCollisionRejected 撞名 claim 被拒且私钥不可交付、台账不被改写。
func TestEnrollNameCollisionRejected(t *testing.T) {
	s, st := newEnrollServer(t)

	// 台账已有 prod-db-01 @ 10.9.9.9
	if _, err := st.CreateHost(&store.Host{Name: "prod-db-01", Address: "10.9.9.9", AgentPort: 7602}); err != nil {
		t.Fatal(err)
	}

	// 未绑定主机名的 token（host 留空，名字由 claim 决定）
	admin := loginSession(t, s)
	rec := do(t, s.Handler(), "POST", "/api/enroll-tokens", map[string]any{}, &admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("生成 token 应 201: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		Token string `json:"token"`
	}
	jsonUnmarshal(t, rec.Body.Bytes(), &created)
	h := s.Handler()

	// 撞名 claim（httptest 来源 192.0.0.1 ≠ 台账 10.9.9.9）→ 409
	rec = do(t, h, "POST", "/enroll/"+created.Token+"/claim",
		map[string]any{"hostname": "prod-db-01", "platform": "linux_amd64"}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("撞名 claim 应 409: %d %s", rec.Code, rec.Body)
	}

	// 被拒的 claim 不得让私钥变得可取（ClaimHost 未落库 → 仍应 410）
	rec = do(t, h, "GET", "/enroll/"+created.Token+"/host-key", nil, nil)
	if rec.Code != http.StatusGone {
		t.Fatalf("被拒 claim 后私钥不应可取: %d %s", rec.Code, rec.Body)
	}

	// done 也不得改写台账（未 claim 的 token → 410，不产生任何落账）
	rec = do(t, h, "POST", "/enroll/"+created.Token+"/done", map[string]any{"agent_port": 7602}, nil)
	if rec.Code != http.StatusGone {
		t.Fatalf("被拒 claim 后 done 应 410: %d %s", rec.Code, rec.Body)
	}
	got, err := st.GetHostByName("prod-db-01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != "10.9.9.9" {
		t.Fatalf("台账地址被改写: %+v", got)
	}
}

// TestEnrollBoundTokenReinstallAllowed 绑定主机名的 token 允许同名异址重装
//（管理员显式授权的迁移/重装路径）。
func TestEnrollBoundTokenReinstallAllowed(t *testing.T) {
	s, st := newEnrollServer(t)
	if _, err := st.CreateHost(&store.Host{Name: "prod-db-01", Address: "10.9.9.9", AgentPort: 7602}); err != nil {
		t.Fatal(err)
	}

	admin := loginSession(t, s)
	rec := do(t, s.Handler(), "POST", "/api/enroll-tokens", map[string]any{"host": "prod-db-01"}, &admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("生成 token 应 201: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		Token string `json:"token"`
	}
	jsonUnmarshal(t, rec.Body.Bytes(), &created)
	h := s.Handler()

	// 同名异址 claim：token 绑定了该主机名 → 放行
	rec = do(t, h, "POST", "/enroll/"+created.Token+"/claim",
		map[string]any{"hostname": "prod-db-01", "platform": "linux_amd64"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("绑定 token 的重装 claim 应 200: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "GET", "/enroll/"+created.Token+"/host-cert", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("绑定 token 的证书交付应 200: %d %s", rec.Code, rec.Body)
	}
}

// jsonUnmarshal 测试辅助（失败即 Fatal）。
func jsonUnmarshal(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
}
