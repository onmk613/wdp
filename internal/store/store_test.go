package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestHostCRUD 主机台账增删改查与迁移幂等。
func TestHostCRUD(t *testing.T) {
	s := openTest(t)

	id, err := s.CreateHost(&Host{Name: "web1", Address: "10.0.0.11", AgentPort: 7602, Groups: []string{"web"}, Labels: `{"env":"prod"}`})
	if err != nil {
		t.Fatal(err)
	}
	// 缺省补齐：端口、labels、状态
	id2, err := s.CreateHost(&Host{Name: "db1", Address: "10.0.1.10"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.GetHost(id2)
	if err != nil {
		t.Fatal(err)
	}
	if h.AgentPort != 7602 || h.Labels != "{}" || h.Status != "unknown" || h.CreatedAt == "" {
		t.Fatalf("缺省补齐异常: %+v", h)
	}

	// 重名拒绝
	if _, err := s.CreateHost(&Host{Name: "web1", Address: "10.0.0.99"}); err == nil {
		t.Fatal("重名应报错")
	}
	// 非法 labels 拒绝
	if _, err := s.CreateHost(&Host{Name: "bad", Address: "10.0.0.98", Labels: `[1,2]`}); err == nil {
		t.Fatal("非对象 labels 应报错")
	}

	hosts, err := s.ListHosts("")
	if err != nil || len(hosts) != 2 {
		t.Fatalf("ListHosts: %v %d", err, len(hosts))
	}

	// 更新（status 不被覆盖）
	if err := s.SetHostStatus(id, "online"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateHost(id, &Host{Address: "10.0.0.12", AgentPort: 7700, Groups: []string{"web2"}, Labels: `{"env":"staging"}`}); err != nil {
		t.Fatal(err)
	}
	h, err = s.GetHost(id)
	if err != nil {
		t.Fatal(err)
	}
	if h.Address != "10.0.0.12" || h.AgentPort != 7700 || !reflect.DeepEqual(h.Groups, []string{"web2"}) {
		t.Fatalf("更新未生效: %+v", h)
	}
	if h.Status != "online" || h.LastSeenAt == "" {
		t.Fatalf("探活状态不应被更新覆盖: %+v", h)
	}
	// 离线不清 last_seen
	if err := s.SetHostStatus(id, "offline"); err != nil {
		t.Fatal(err)
	}
	h, _ = s.GetHost(id)
	if h.Status != "offline" || h.LastSeenAt == "" {
		t.Fatalf("offline 应保留 last_seen: %+v", h)
	}

	// HostIDs
	ids, err := s.HostIDs()
	if err != nil || len(ids) != 2 {
		t.Fatalf("HostIDs: %v %v", ids, err)
	}

	// 删除与不存在
	if err := s.DeleteHost(id2); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteHost(id2); err != ErrNotFound {
		t.Fatalf("重复删除应 ErrNotFound: %v", err)
	}
	if _, err := s.GetHost(id2); err != ErrNotFound {
		t.Fatalf("查询已删主机应 ErrNotFound: %v", err)
	}
}

// TestMigrateIdempotent 重复 Open 同一库不再重放迁移、数据保留。
func TestMigrateIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateHost(&Host{Name: "web1", Address: "10.0.0.11"}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	hosts, err := s2.ListHosts("")
	if err != nil || len(hosts) != 1 {
		t.Fatalf("重开库数据应保留: %v %d", err, len(hosts))
	}
}

// TestUsers 账号创建与查询。
func TestUsers(t *testing.T) {
	s := openTest(t)
	if n, _ := s.CountUsers(); n != 0 {
		t.Fatalf("空库账号数应为 0: %d", n)
	}
	if err := s.CreateUser("admin", "$2a$10$examplehash", "admin"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountUsers(); n != 1 {
		t.Fatalf("账号数应为 1: %d", n)
	}
	u, err := s.UserByName("admin")
	if err != nil || u.PasswordHash != "$2a$10$examplehash" {
		t.Fatalf("UserByName: %+v %v", u, err)
	}
	if _, err := s.UserByName("nobody"); err != ErrNotFound {
		t.Fatalf("不存在账号应 ErrNotFound: %v", err)
	}
}

// TestSetUserPassword 密码 upsert：不存在创建、存在重置。
func TestSetUserPassword(t *testing.T) {
	s := openTest(t)
	if err := s.SetUserPassword("admin", "hash-1"); err != nil {
		t.Fatal(err)
	}
	u, _ := s.UserByName("admin")
	if u.PasswordHash != "hash-1" {
		t.Fatalf("创建后散列异常: %s", u.PasswordHash)
	}
	if err := s.SetUserPassword("admin", "hash-2"); err != nil {
		t.Fatal(err)
	}
	u, _ = s.UserByName("admin")
	if u.PasswordHash != "hash-2" {
		t.Fatalf("重置后散列异常: %s", u.PasswordHash)
	}
	if err := s.SetUserPassword("", "h"); err == nil {
		t.Fatal("空用户名应报错")
	}
}

// TestListRegistriesEmptyAndMembers 空注册表返回非 nil（JSON []），
// 池列表附带成员数；重复名报可读错误。
func TestListRegistriesEmptyAndMembers(t *testing.T) {
	s := openTest(t)

	pools, err := s.ListPools()
	if err != nil || pools == nil || len(pools) != 0 {
		t.Fatalf("空池列表应为 []: %v %v", pools, err)
	}
	groups, _ := s.ListGroups()
	labels, _ := s.ListLabels()
	if groups == nil || labels == nil {
		t.Fatal("空组/标签列表应为非 nil")
	}
	hosts, _ := s.ListHosts("")
	if hosts == nil {
		t.Fatal("空台账应为非 nil（JSON []）")
	}

	// 建池 + 划入两台主机 → Members=2
	id1, _ := s.CreateHost(&Host{Name: "h1", Address: "10.0.0.1"})
	id2, _ := s.CreateHost(&Host{Name: "h2", Address: "10.0.0.2"})
	if _, err := s.CreatePool("prod", "", []int64{id1, id2}); err != nil {
		t.Fatal(err)
	}
	pools, _ = s.ListPools()
	if len(pools) != 1 || pools[0].Members != 2 {
		t.Fatalf("成员数异常: %+v", pools)
	}

	// 重复名：可读错误（非原始 SQLite 约束串）
	if _, err := s.CreatePool("prod", "", nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("重复池名应报已存在: %v", err)
	}
	if _, err := s.CreateGroup("web", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGroup("web", "", nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("重复组名应报已存在: %v", err)
	}
	if _, err := s.CreateLabel("env", "", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLabel("env", "", nil, ""); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("重复标签键应报已存在: %v", err)
	}
}

// TestEnrollTokenLifecycle 纳管凭证：创建 → 查询 → claim（幂等）→
// 消费（单次）→ 过期拒绝；过期清理随创建进行。
func TestEnrollTokenLifecycle(t *testing.T) {
	s := openTest(t)

	if err := s.CreateEnrollToken("tok1", "web9", 7700, time.Hour); err != nil {
		t.Fatal(err)
	}
	t1, err := s.GetEnrollToken("tok1")
	if err != nil || t1.HostName != "web9" || t1.AgentPort != 7700 || t1.UsedAt != "" {
		t.Fatalf("GetEnrollToken: %+v %v", t1, err)
	}

	// claim：记录 hostname/地址，幂等
	if _, err := s.ClaimEnrollToken("tok1", "web9", "10.0.0.9"); err != nil {
		t.Fatal(err)
	}
	t1, _ = s.GetEnrollToken("tok1")
	if t1.ClaimHost != "web9" || t1.ClaimAddress != "10.0.0.9" {
		t.Fatalf("claim 未记录: %+v", t1)
	}
	if _, err := s.ClaimEnrollToken("tok1", "web9", "10.0.0.9"); err != nil {
		t.Fatalf("重复 claim 幂等应通过: %v", err)
	}

	// 消费：单次
	if _, err := s.ConsumeEnrollToken("tok1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeEnrollToken("tok1"); err != ErrTokenUsed {
		t.Fatalf("重复消费应 ErrTokenUsed: %v", err)
	}
	if _, err := s.ClaimEnrollToken("tok1", "web9", "10.0.0.9"); err != ErrTokenUsed {
		t.Fatalf("已用 token claim 应 ErrTokenUsed: %v", err)
	}

	// 过期拒绝
	if err := s.CreateEnrollToken("tok2", "", 0, -time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimEnrollToken("tok2", "h", "10.0.0.2"); err != ErrTokenExpired {
		t.Fatalf("过期 claim 应 ErrTokenExpired: %v", err)
	}
	if _, err := s.ConsumeEnrollToken("tok2"); err != ErrTokenExpired {
		t.Fatalf("过期消费应 ErrTokenExpired: %v", err)
	}

	// 未知 token
	if _, err := s.GetEnrollToken("nope"); err != ErrNotFound {
		t.Fatalf("未知 token 应 ErrNotFound: %v", err)
	}

	// ListEnrollTokens 只列未过期；创建顺带清理过期未用凭证
	if err := s.CreateEnrollToken("tok3", "", 0, time.Hour); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListEnrollTokens()
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range list {
		if tk.Token == "tok2" {
			t.Fatal("过期凭证不应出现在列表（创建时清理）")
		}
	}
}

// TestUpsertHostByName 纳管落账：新建与同名更新（保留分组/标签）。
func TestUpsertHostByName(t *testing.T) {
	s := openTest(t)
	id1, err := s.UpsertHostByName("web9", "10.0.0.9", 7602)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.UpsertHostByName("web9", "10.0.0.10", 7700)
	if err != nil || id2 != id1 {
		t.Fatalf("同名应更新而非新建: %d %d %v", id1, id2, err)
	}
	h, _ := s.GetHost(id1)
	if h.Address != "10.0.0.10" || h.AgentPort != 7700 {
		t.Fatalf("更新未生效: %+v", h)
	}
	if _, err := s.UpsertHostByName("", "10.0.0.9", 7602); err == nil {
		t.Fatal("空名应报错")
	}
}

// TestVersionScopes 版本级 scope 的读取语义：正常读回按版本保存的池/组/
// 标签；迁移前旧行（列为空串）返回 ok=false 供上层回退应用级；
// UpdateAppScopes 同步写默认版本。
func TestVersionScopes(t *testing.T) {
	s := openTest(t)
	id, err := s.CreateApp("scope-app", "", `{"env":"prod"}`, []string{"pool-a"}, []string{"grp-a"}, "1.0.0", "/tmp/x.tgz", "sha", 1, []string{"deploy", "uninstall"})
	if err != nil {
		t.Fatal(err)
	}
	p, g, l, ok, err := s.VersionScopes(id, "1.0.0")
	if err != nil || !ok || len(p) != 1 || p[0] != "pool-a" || len(g) != 1 || g[0] != "grp-a" || l != `{"env":"prod"}` {
		t.Fatalf("版本 scope 读回异常: %v %v %v %v %v", p, g, l, ok, err)
	}
	if ph, err := s.VersionPhases(id, "1.0.0"); err != nil || len(ph) != 2 || ph[0] != "deploy" || ph[1] != "uninstall" {
		t.Fatalf("版本相位清单读回异常: %v %v", ph, err)
	}
	if _, _, _, ok, err := s.VersionScopes(id, "9.9.9"); err == nil || ok {
		t.Fatalf("不存在版本应 ErrNotFound: %v %v", ok, err)
	}

	// 旧行（迁移前数据）：列值为空串 → ok=false（上层回退应用级）
	if _, err := s.db.Exec(`UPDATE app_versions SET pools = '', groups = ''`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := s.VersionScopes(id, "1.0.0"); err != nil || ok {
		t.Fatalf("旧行应回退（ok=false）: %v %v", ok, err)
	}

	// UpdateAppScopes 应同步默认版本行
	if _, err := s.db.Exec(`UPDATE app_versions SET pools = '[]', groups = '[]'`); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAppScopes(id, "n", []string{"pool-z"}, nil, `{}`); err != nil {
		t.Fatal(err)
	}
	p, _, _, ok, err = s.VersionScopes(id, "1.0.0")
	if err != nil || !ok || len(p) != 1 || p[0] != "pool-z" {
		t.Fatalf("默认版本应同步应用级 scope: %v %v %v", p, ok, err)
	}
}

// TestDeleteHostCleansMappings 删主机须同事务清掉池/组映射：残留行会把
// 已删主机继续计入池成员数并永久累积。
func TestDeleteHostCleansMappings(t *testing.T) {
	s := openTest(t)
	hid, err := s.CreateHost(&Host{Name: "web1", Address: "10.0.0.11", Pools: []string{"prod"}, Groups: []string{"web"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePool("prod", "", nil); err != nil {
		t.Fatal(err)
	}
	pools, _ := s.ListPools()
	if len(pools) != 1 || pools[0].Members != 1 {
		t.Fatalf("删除前成员数应为 1: %+v", pools)
	}
	if err := s.DeleteHost(hid); err != nil {
		t.Fatal(err)
	}
	pools, _ = s.ListPools()
	if len(pools) != 1 || pools[0].Members != 0 {
		t.Fatalf("删除主机后成员数应为 0: %+v", pools)
	}
	var n int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM host_pools) + (SELECT COUNT(*) FROM host_group_map)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("映射表应无残留行: n=%d err=%v", n, err)
	}
}

// TestTxRollbackNoPartialWrite 事务辅助：fn 失败时不得留下任何写入。
func TestTxRollbackNoPartialWrite(t *testing.T) {
	s := openTest(t)
	id, err := s.CreateHost(&Host{Name: "web1", Address: "10.0.0.11"})
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	err = s.tx(func(q execer) error {
		if _, err := q.Exec(`UPDATE hosts SET address = '10.9.9.9' WHERE id = ?`, id); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("应透传 fn 的错误: %v", err)
	}
	h, err := s.GetHost(id)
	if err != nil || h.Address != "10.0.0.11" {
		t.Fatalf("回滚后不应有任何写入: %+v %v", h, err)
	}
}

// TestReplaceUserScopesRollback 整体替换在 INSERT 失败时旧授权不丢
// （触发器模拟写入故障，验证事务回滚）。
func TestReplaceUserScopesRollback(t *testing.T) {
	s := openTest(t)
	if err := s.CreateUser("u1", "h", "viewer"); err != nil {
		t.Fatal(err)
	}
	u, err := s.UserByName("u1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceUserScopes(u.ID, []*UserScope{{Verb: "host:view", Kind: "pool", Value: "prod"}}); err != nil {
		t.Fatal(err)
	}
	// RAISE(ABORT) 显式指定冲突处理，INSERT OR IGNORE 不吞
	if _, err := s.db.Exec(`CREATE TRIGGER deny_scope_insert BEFORE INSERT ON user_scopes BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.db.Exec(`DROP TRIGGER deny_scope_insert`) })
	if err := s.ReplaceUserScopes(u.ID, []*UserScope{{Verb: "host:edit"}}); err == nil {
		t.Fatal("INSERT 失败应返回错误")
	}
	got, err := s.UserScopes(u.ID)
	if err != nil || len(got) != 1 || got[0].Verb != "host:view" {
		t.Fatalf("回滚后旧授权应保留: %+v %v", got, err)
	}
}

// TestConsumeEnrollTokenDoubleSpend 前置检查通过但 UPDATE 未命中（被并发
// 抢先标记）时应按已使用拒绝，不得双花。
func TestConsumeEnrollTokenDoubleSpend(t *testing.T) {
	s := openTest(t)
	if err := s.CreateEnrollToken("tok", "h", 0, time.Hour); err != nil {
		t.Fatal(err)
	}
	// RAISE(IGNORE) 让 UPDATE 空过（RowsAffected=0），模拟双花窗口
	if _, err := s.db.Exec(`CREATE TRIGGER skip_mark_used BEFORE UPDATE ON enroll_tokens BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.db.Exec(`DROP TRIGGER skip_mark_used`) })
	if _, err := s.ConsumeEnrollToken("tok"); err != ErrTokenUsed {
		t.Fatalf("UPDATE 未命中应报 ErrTokenUsed: %v", err)
	}
	tk, err := s.GetEnrollToken("tok")
	if err != nil || tk.UsedAt != "" {
		t.Fatalf("拒绝路径不应标记已使用: %+v %v", tk, err)
	}
}

// TestClaimEnrollTokenMismatch 幂等仅限信息一致：同 token 换 hostname/
// 来源的重复 claim 按重放拒绝。
func TestClaimEnrollTokenMismatch(t *testing.T) {
	s := openTest(t)
	if err := s.CreateEnrollToken("tok", "", 0, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimEnrollToken("tok", "web9", "10.0.0.9"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimEnrollToken("tok", "other", "10.0.0.9"); err == nil {
		t.Fatal("hostname 不一致的重复 claim 应报错")
	}
	if _, err := s.ClaimEnrollToken("tok", "web9", "10.0.0.8"); err == nil {
		t.Fatal("来源不一致的重复 claim 应报错")
	}
	if _, err := s.ClaimEnrollToken("tok", "web9", "10.0.0.9"); err != nil {
		t.Fatalf("信息一致的重复 claim 应幂等放行: %v", err)
	}
}

// TestScopeNameValidation 池/组/标签键及主机/应用侧归属名含逗号/空白
// 一律拒绝（group_concat 按逗号拆分，坏名字会拆错）。
func TestScopeNameValidation(t *testing.T) {
	s := openTest(t)
	for _, name := range []string{"a,b", "a b", "a\tb", "a\nb", " "} {
		if _, err := s.CreatePool(name, "", nil); err == nil {
			t.Fatalf("池名 %q 应被拒绝", name)
		}
		if _, err := s.CreateGroup(name, "", nil); err == nil {
			t.Fatalf("组名 %q 应被拒绝", name)
		}
		if _, err := s.CreateLabel(name, "", nil, ""); err == nil {
			t.Fatalf("标签键 %q 应被拒绝", name)
		}
	}
	if _, err := s.CreateHost(&Host{Name: "h1", Address: "10.0.0.1", Pools: []string{"a,b"}}); err == nil {
		t.Fatal("主机池归属名含逗号应被拒绝")
	}
	if _, err := s.CreateHost(&Host{Name: "h2", Address: "10.0.0.2", Groups: []string{"a b"}}); err == nil {
		t.Fatal("主机组归属名含空白应被拒绝")
	}
	if _, err := s.CreateApp("app1", "", "{}", []string{"a,b"}, nil, "1.0.0", "/tmp/x", "sha", 1, nil); err == nil {
		t.Fatal("应用池名含逗号应被拒绝")
	}
}

// TestHostScopesSorted 池/组读回按名排序，输出稳定。
func TestHostScopesSorted(t *testing.T) {
	s := openTest(t)
	if _, err := s.CreateHost(&Host{Name: "web1", Address: "10.0.0.11",
		Pools:  []string{"c-pool", "a-pool", "b-pool"},
		Groups: []string{"z-group", "a-group"}}); err != nil {
		t.Fatal(err)
	}
	h, err := s.GetHost(1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.Pools, []string{"a-pool", "b-pool", "c-pool"}) || !reflect.DeepEqual(h.Groups, []string{"a-group", "z-group"}) {
		t.Fatalf("池/组读回应排序稳定: %+v", h)
	}
}

// TestUpsertHostByNameConcurrent 并发同名纳管：均成功且落到同一台账行
// （唯一冲突重试，不向调用方抛裸约束错）。
func TestUpsertHostByNameConcurrent(t *testing.T) {
	s := openTest(t)
	const n = 8
	ids := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = s.UpsertHostByName("web9", "10.0.0.9", 7602)
		}(i)
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil {
			t.Fatalf("并发纳管 #%d 不应报错: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("并发纳管应落到同一台账行: %v", ids)
		}
	}
	hosts, err := s.ListHosts("")
	if err != nil || len(hosts) != 1 {
		t.Fatalf("应只有一行台账: %d %v", len(hosts), err)
	}
}

// TestDeleteVersionRecomputeLatest 删最新版后 latest 回退到剩余最新版，
// 全删清空；制品路径原样返回给调用方清理。
func TestDeleteVersionRecomputeLatest(t *testing.T) {
	s := openTest(t)
	id, err := s.CreateApp("app", "", "{}", nil, nil, "1.0.0", "/tmp/a.tgz", "sha", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct{ ver, tgz string }{{"2.0.0", "/tmp/b.tgz"}, {"3.0.0", "/tmp/c.tgz"}} {
		if err := s.AddVersion(id, v.ver, v.tgz, "sha", 1, "", nil, nil, "{}", nil); err != nil {
			t.Fatal(err)
		}
	}
	vid := func(version string) int64 {
		vs, err := s.ListVersions(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range vs {
			if v.Version == version {
				return v.ID
			}
		}
		t.Fatalf("版本 %s 不存在", version)
		return 0
	}
	a, _ := s.GetApp(id)
	if a.LatestVersion != "3.0.0" {
		t.Fatalf("初始 latest 应为 3.0.0: %s", a.LatestVersion)
	}
	if tgz, err := s.DeleteVersion(id, vid("3.0.0")); err != nil || tgz != "/tmp/c.tgz" {
		t.Fatalf("删最新版: tgz=%q err=%v", tgz, err)
	}
	if a, _ = s.GetApp(id); a.LatestVersion != "2.0.0" {
		t.Fatalf("latest 应回退到 2.0.0: %s", a.LatestVersion)
	}
	if _, err := s.DeleteVersion(id, vid("2.0.0")); err != nil {
		t.Fatal(err)
	}
	if a, _ = s.GetApp(id); a.LatestVersion != "1.0.0" {
		t.Fatalf("latest 应回退到 1.0.0: %s", a.LatestVersion)
	}
	if tgz, err := s.DeleteVersion(id, vid("1.0.0")); err != nil || tgz != "/tmp/a.tgz" {
		t.Fatalf("删末版: tgz=%q err=%v", tgz, err)
	}
	if a, _ = s.GetApp(id); a.LatestVersion != "" {
		t.Fatalf("清空后 latest 应为空: %s", a.LatestVersion)
	}
}

// TestDeleteRunCleansTasks store 层删除执行记录级联清理任务明细。
func TestDeleteRunCleansTasks(t *testing.T) {
	s := openTest(t)
	rid, err := s.CreateRun("exec", 0, "", "", "", 0, "all", "admin", "running")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddRunTask(&RunTask{RunID: rid, Host: "h1", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRun(rid); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.RunTasks(rid)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("任务明细应级联清理: %d %v", len(tasks), err)
	}
}
