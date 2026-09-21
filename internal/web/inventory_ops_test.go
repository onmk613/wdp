package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"wdp/internal/store"
)

// TestInventoryOps 池/组/标签注册表全链路 + 搜索 + 批量设置。
func TestInventoryOps(t *testing.T) {
	s, st := newEnrollServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	// 造两台主机（地址无前缀包含关系）
	for _, hd := range []struct{ name, addr string }{{"web1", "10.0.1.1"}, {"db1", "10.0.2.2"}} {
		rec := do(t, h, "POST", "/api/hosts", map[string]any{
			"Name": hd.name, "Address": hd.addr, "AgentPort": 7602,
		}, &token)
		if rec.Code != http.StatusCreated {
			t.Fatalf("创建 %s 应 201: %d %s", hd.name, rec.Code, rec.Body)
		}
	}

	// 新建两个池（划入 web1）——多值：一台主机可属多个池
	for _, pn := range []string{"prod-pool", "edge-pool"} {
		if rec := do(t, h, "POST", "/api/pools", map[string]any{"name": pn, "note": "生产", "host_ids": []int64{1}}, &token); rec.Code != http.StatusCreated {
			t.Fatalf("新建池 %s: %d %s", pn, rec.Code, rec.Body)
		}
	}
	// 新建组（划入两台）
	if rec := do(t, h, "POST", "/api/groups", map[string]any{"name": "web", "host_ids": []int64{1, 2}}, &token); rec.Code != http.StatusCreated {
		t.Fatalf("新建组: %d %s", rec.Code, rec.Body)
	}
	// 新建标签（附加到 web1）
	if rec := do(t, h, "POST", "/api/labels", map[string]any{"key": "env", "value": "prod", "host_ids": []int64{1}}, &token); rec.Code != http.StatusCreated {
		t.Fatalf("新建标签: %d %s", rec.Code, rec.Body)
	}
	// 空键拒绝
	if rec := do(t, h, "POST", "/api/labels", map[string]any{"key": ""}, &token); rec.Code != http.StatusBadRequest {
		t.Fatalf("空键应 400: %d", rec.Code)
	}

	// 主机带上了池/组/标签
	h1, _ := st.GetHost(1)
	if !equalSet(h1.Pools, []string{"prod-pool", "edge-pool"}) || !reflect.DeepEqual(h1.Groups, []string{"web"}) || !strings.Contains(h1.Labels, `"env":"prod"`) {
		t.Fatalf("主机归属异常（应多池单组）: %+v", h1)
	}

	// 注册表列表端点真的返回数据（曾因 Scan 列数不符返回 500 而前端静默）
	rec := do(t, h, "GET", "/api/pools", nil, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/pools 应 200: %d %s", rec.Code, rec.Body)
	}
	var poolList []*store.Pool
	json.Unmarshal(rec.Body.Bytes(), &poolList)
	if len(poolList) != 2 || poolList[0].Name != "edge-pool" || poolList[0].Members != 1 {
		t.Fatalf("池列表异常: %s", rec.Body)
	}
	rec = do(t, h, "GET", "/api/groups", nil, &token)
	var groupList []*store.GroupEntry
	json.Unmarshal(rec.Body.Bytes(), &groupList)
	if len(groupList) != 1 || groupList[0].Name != "web" {
		t.Fatalf("组列表异常: %s", rec.Body)
	}
	rec = do(t, h, "GET", "/api/labels", nil, &token)
	var labelList []*store.LabelDef
	json.Unmarshal(rec.Body.Bytes(), &labelList)
	if len(labelList) != 1 || labelList[0].Key != "env" {
		t.Fatalf("标签列表异常: %s", rec.Body)
	}
	// 重复名：可读错误
	if rec := do(t, h, "POST", "/api/pools", map[string]any{"name": "prod-pool"}, &token); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("重复池名应可读报错: %d %s", rec.Code, rec.Body)
	}

	// 搜索：按池 / 标签值 / IP 前缀
	for _, q := range []string{"edge-pool", "prod", "10.0.1.1"} {
		rec := do(t, h, "GET", "/api/hosts?q="+q, nil, &token)
		var list []*store.Host
		json.Unmarshal(rec.Body.Bytes(), &list)
		if len(list) != 1 || list[0].Name != "web1" {
			t.Fatalf("搜索 %q 应命中 web1: %s", q, rec.Body)
		}
	}
	rec = do(t, h, "GET", "/api/hosts?q=web1", nil, &token)
	var all []*store.Host
	json.Unmarshal(rec.Body.Bytes(), &all)
	if len(all) != 1 {
		t.Fatalf("按名搜索应 1 台: %s", rec.Body)
	}

	// 批量 assign：db1 设池、加标签
	rec = do(t, h, "POST", "/api/hosts/batch", BatchRequest{
		IDs: []int64{2}, Action: "assign", Pools: []string{"db-pool", "backup"}, Groups: []string{}, SetPools: true, SetGroups: true, Labels: map[string]string{"env": "dev"},
	}, &token)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":1`) {
		t.Fatalf("批量 assign: %d %s", rec.Code, rec.Body)
	}
	h2, _ := st.GetHost(2)
	if !equalSet(h2.Pools, []string{"db-pool", "backup"}) || len(h2.Groups) != 0 || !strings.Contains(h2.Labels, `"env":"dev"`) {
		t.Fatalf("批量 assign 未生效: %+v", h2)
	}

	// 空 ids 拒绝 / 未知 action 拒绝
	if rec := do(t, h, "POST", "/api/hosts/batch", BatchRequest{Action: "probe"}, &token); rec.Code != http.StatusBadRequest {
		t.Fatalf("空 ids 应 400: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/api/hosts/batch", BatchRequest{IDs: []int64{1}, Action: "boom"}, &token); rec.Code != http.StatusBadRequest {
		t.Fatalf("未知 action 应 400: %d", rec.Code)
	}

	// 删除池 → 成员解除归属（主机保留）
	if rec := do(t, h, "DELETE", "/api/pools/1", nil, &token); rec.Code != http.StatusOK {
		t.Fatalf("删池: %d %s", rec.Code, rec.Body)
	}
	h1, _ = st.GetHost(1)
	if !reflect.DeepEqual(h1.Pools, []string{"edge-pool"}) {
		t.Fatalf("删池后其余归属应保留: %+v", h1)
	}
	// 删除标签 → 从主机移除键
	if rec := do(t, h, "DELETE", "/api/labels/1", nil, &token); rec.Code != http.StatusOK {
		t.Fatalf("删标签: %d %s", rec.Code, rec.Body)
	}
	h1, _ = st.GetHost(1)
	if strings.Contains(h1.Labels, "env") {
		t.Fatalf("删标签后键应移除: %s", h1.Labels)
	}
}

// TestImportHosts 批量建档：逐台结果（重名行不阻断后续）、name 缺省取
// address、labels 落库、空数组拒绝。
func TestImportHosts(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	rec := do(t, h, "POST", "/api/hosts/import", map[string]any{"hosts": []map[string]any{
		{"name": "web1", "address": "10.1.0.11", "pools": []string{"web", "prod"}, "groups": []string{"api"}, "labels": map[string]string{"env": "prod", "rack": "a1"}},
		{"address": "10.1.0.12"},                 // name 缺省 = address
		{"name": "web1", "address": "10.1.0.13"}, // 与首行重名 → 失败，不阻断
	}}, &token)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":2`) || !strings.Contains(rec.Body.String(), `"failed":1`) {
		t.Fatalf("导入应 2 成 1 败: %d %s", rec.Code, rec.Body)
	}
	h1, err := st.GetHost(1)
	if err != nil || h1.Name != "web1" || !equalSet(h1.Pools, []string{"web", "prod"}) || !strings.Contains(h1.Labels, `"rack":"a1"`) {
		t.Fatalf("导入主机 1 异常: %+v err=%v", h1, err)
	}
	h2, err := st.GetHost(2)
	if err != nil || h2.Name != "10.1.0.12" || h2.AgentPort != 7602 {
		t.Fatalf("name 缺省应取 address、端口缺省 7602: %+v err=%v", h2, err)
	}
	if _, err := st.GetHost(3); err == nil {
		t.Fatal("重名行不应建档")
	}

	if rec := do(t, h, "POST", "/api/hosts/import", map[string]any{"hosts": []map[string]any{}}, &token); rec.Code != http.StatusBadRequest {
		t.Fatalf("空数组应 400: %d", rec.Code)
	}
}

// TestDeleteHostRetiresAgent 删除主机时向 agent 发 /shutdown
// （本测试无 agent 在线 → 退役失败仅告警，台账照删）。
func TestDeleteHostRetiresAgent(t *testing.T) {
	s, st := newEnrollServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	rec := do(t, h, "POST", "/api/hosts", map[string]any{"Name": "gone", "Address": "127.0.0.1", "AgentPort": 9}, &token)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建: %d", rec.Code)
	}
	rec = do(t, h, "DELETE", "/api/hosts/1", nil, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除应 200: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		OK           bool   `json:"ok"`
		AgentRetired bool   `json:"agent_retired"`
		Warning      string `json:"warning"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.OK || resp.AgentRetired {
		t.Fatalf("离线主机：应删除成功且退役失败: %+v", resp)
	}
	if resp.Warning == "" {
		t.Fatal("退役失败应带 warning")
	}
	if _, err := st.GetHost(1); err != store.ErrNotFound {
		t.Fatalf("台账应已删除: %v", err)
	}
}

// equalSet 顺序无关的字符串集合比较。
func equalSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([]string(nil), a...)
	sb := append([]string(nil), b...)
	slices.Sort(sa)
	slices.Sort(sb)
	return slices.Equal(sa, sb)
}

// TestImportHostsValidation 逐行校验：非法 name 字符、空 address、端口
// 越界均拒绝该行（不阻断后续行）。
func TestImportHostsValidation(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)
	rec := do(t, h, "POST", "/api/hosts/import", map[string]any{"hosts": []map[string]any{
		{"name": "bad/name", "address": "10.1.0.1"},
		{"name": "noaddr"},
		{"name": "badport", "address": "10.1.0.2", "agent_port": 70000},
		{"name": "good", "address": "10.1.0.3"},
	}}, &token)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":1`) || !strings.Contains(rec.Body.String(), `"failed":3`) {
		t.Fatalf("导入应 1 成 3 败: %d %s", rec.Code, rec.Body)
	}
	hosts, _ := st.ListHosts("")
	if len(hosts) != 1 || hosts[0].Name != "good" {
		t.Fatalf("仅合法行应建档: %+v", hosts)
	}
}

// TestBatchAssignScopeCheck 作用域级 host:edit 不能把主机 assign 到未授权
// 池/组（写入校验：目标值须被授权全覆盖，"任一命中"不够）。
func TestBatchAssignScopeCheck(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	admin := loginSession(t, s)
	id, err := st.CreateHost(&store.Host{Name: "assign-h", Address: "127.0.0.1", AgentPort: 1, Pools: []string{"pool-a"}})
	if err != nil {
		t.Fatal(err)
	}
	scoped := newUserSession(t, h, admin, "assignsc", "scoped-Pass1", "viewer")
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/users/%d/scopes", userIDByName(t, st, "assignsc")), map[string]any{
		"scopes": []map[string]any{{"verb": "host:edit", "kind": "pool", "value": "pool-a"}},
	}, &admin); rec.Code != http.StatusOK {
		t.Fatalf("追加授权应 200: %d %s", rec.Code, rec.Body)
	}
	// 主机本身在授权范围内，但目标池越权 → 403
	if rec := do(t, h, "POST", "/api/hosts/batch", BatchRequest{IDs: []int64{id}, Action: "assign", Pools: []string{"pool-b"}, SetPools: true}, &scoped); rec.Code != http.StatusForbidden {
		t.Fatalf("assign 到未授权池应 403: %d %s", rec.Code, rec.Body)
	}
	// 改到授权池 → 正常
	if rec := do(t, h, "POST", "/api/hosts/batch", BatchRequest{IDs: []int64{id}, Action: "assign", Pools: []string{"pool-a"}, SetPools: true}, &scoped); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":1`) {
		t.Fatalf("assign 到授权池应成功: %d %s", rec.Code, rec.Body)
	}
	// 不动池/组（SetPools/SetGroups 均未置）只加标签 → 不触发归属写入校验
	if rec := do(t, h, "POST", "/api/hosts/batch", BatchRequest{IDs: []int64{id}, Action: "assign", Labels: map[string]string{"env": "dev"}}, &scoped); rec.Code != http.StatusOK {
		t.Fatalf("仅标签 assign 应成功: %d %s", rec.Code, rec.Body)
	}
}
