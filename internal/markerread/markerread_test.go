package markerread

// 共享核心的口径回归：三分类边界（含 v2 缺 values 字段的 legacy 并集
// 口径）、hosts 声明序聚合、Validate 的去重归因稳定性与序列化失败单独
// 报错（CLI 旧版吞 json.Marshal 错误的 bug 在此锁定修复）。

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wdp/internal/chart"
	"wdp/internal/conn"
	"wdp/internal/conn/fake"
	"wdp/internal/model"
)

// fakeDial 按主机名路由到预置的假连接（释放动作计数进 released）。
func fakeDial(conns map[string]*fake.Fake, released *int32) ConnFactory {
	return func(_ context.Context, h *model.Host) (conn.Conn, func(), error) {
		c, ok := conns[h.Name]
		if !ok {
			return nil, nil, fmt.Errorf("host %s connection failed: no fake", h.Name)
		}
		return c, func() { atomic.AddInt32(released, 1) }, nil
	}
}

func host(name string) *model.Host { return &model.Host{Name: name, Conn: "fake"} }

// markerV2 构造 v2 marker JSON（values 为 nil 时省略 values 字段——
// "v2 却缺 values" 的 legacy 边界）。
func markerV2(values string) string {
	if values == "" {
		return `{"chart":"c","version":"1","marker_schema":2}`
	}
	return `{"chart":"c","version":"1","marker_schema":2,"values":` + values + `}`
}

// TestReadClassification 验证四类结果与成因字段：ok / missing（哨兵与
// 空输出）/ legacy（v1 与 v2 缺 values）/ failed（建连错、退出码非零、
// 解析失败），且 release 每主机恰好调用一次。
func TestReadClassification(t *testing.T) {
	conns := map[string]*fake.Fake{
		"ok": {ExecFn: func(conn.ExecRequest) (conn.ExecResult, error) {
			return conn.ExecResult{Code: 0, Stdout: markerV2(`{"data_dir":"/a"}`)}, nil
		}},
		"empty": {ExecFn: func(conn.ExecRequest) (conn.ExecResult, error) {
			return conn.ExecResult{Code: 0, Stdout: "  \n"}, nil
		}},
		"missing": {ExecFn: func(conn.ExecRequest) (conn.ExecResult, error) {
			return conn.ExecResult{Code: 0, Stdout: "__MISSING__"}, nil
		}},
		"v1": {ExecFn: func(conn.ExecRequest) (conn.ExecResult, error) {
			return conn.ExecResult{Code: 0, Stdout: `{"chart":"c","version":"1"}`}, nil
		}},
		"v2novalues": {ExecFn: func(conn.ExecRequest) (conn.ExecResult, error) {
			return conn.ExecResult{Code: 0, Stdout: markerV2("")}, nil
		}},
		"rc": {ExecFn: func(conn.ExecRequest) (conn.ExecResult, error) {
			return conn.ExecResult{Code: 2, Stderr: "boom\nsecond line"}, nil
		}},
		"badjson": {ExecFn: func(conn.ExecRequest) (conn.ExecResult, error) {
			return conn.ExecResult{Code: 0, Stdout: "{not json"}, nil
		}},
	}
	hosts := []*model.Host{host("ok"), host("empty"), host("missing"), host("v1"),
		host("v2novalues"), host("rc"), host("badjson"), host("dead")}
	var released int32
	results := Read(context.Background(), "/var/lib/wdp/c/release.json", hosts,
		fakeDial(conns, &released), Options{Concurrency: 4})

	want := []struct {
		name string
		kind Kind
	}{
		{"ok", KindOK}, {"empty", KindMissing}, {"missing", KindMissing},
		{"v1", KindLegacy}, {"v2novalues", KindLegacy},
		{"rc", KindFailed}, {"badjson", KindFailed}, {"dead", KindFailed},
	}
	for i, w := range want {
		if results[i].Kind != w.kind {
			t.Fatalf("host %s: kind = %d, want %d", w.name, results[i].Kind, w.kind)
		}
	}
	if results[5].Code != 2 || results[5].Stderr != "boom\nsecond line" {
		t.Fatalf("非零退出应携带 Code/Stderr: %+v", results[5])
	}
	if results[6].ParseErr == nil {
		t.Fatalf("坏 marker 应携带 ParseErr: %+v", results[6])
	}
	if results[7].Err == nil || !strings.Contains(results[7].Err.Error(), "connection failed") {
		t.Fatalf("建连失败应携带 Err: %+v", results[7])
	}
	if results[0].Marker == nil || results[0].Marker.Values["data_dir"] != "/a" {
		t.Fatalf("ok 结果应携带 Marker: %+v", results[0])
	}
	// 7 台建连成功（dead 之外）各释放一次
	if int(released) != 7 {
		t.Fatalf("release 应每主机一次(7), got %d", released)
	}
}

// TestReadHonorsRequestMeta 脚本逐字一致（cat + 哨兵）、become root、
// timeout 30s、并发上限生效。
func TestReadHonorsRequestMeta(t *testing.T) {
	var inflight, maxInflight int32
	conns := map[string]*fake.Fake{}
	hosts := make([]*model.Host, 6)
	for i := range hosts {
		name := "h" + strconv.Itoa(i)
		hosts[i] = host(name)
		conns[name] = &fake.Fake{ExecFn: func(req conn.ExecRequest) (conn.ExecResult, error) {
			if req.BecomeUser != "root" {
				t.Errorf("become 应为 root: %q", req.BecomeUser)
			}
			if req.TimeoutMs != 30_000 {
				t.Errorf("timeout_ms 应为 30000: %d", req.TimeoutMs)
			}
			if req.Script != "cat -- '/var/lib/wdp/c/release.json' 2>/dev/null || echo __MISSING__" {
				t.Errorf("脚本应与旧实现逐字一致: %q", req.Script)
			}
			cur := atomic.AddInt32(&inflight, 1)
			for {
				old := atomic.LoadInt32(&maxInflight)
				if cur <= old || atomic.CompareAndSwapInt32(&maxInflight, old, cur) {
					break
				}
			}
			atomic.AddInt32(&inflight, -1)
			return conn.ExecResult{Code: 0, Stdout: "__MISSING__"}, nil
		}}
	}
	results := Read(context.Background(), "/var/lib/wdp/c/release.json", hosts,
		fakeDial(conns, new(int32)), Options{Concurrency: 3})
	for i, r := range results {
		if r.Kind != KindMissing {
			t.Fatalf("host %d: kind = %d, want missing", i, r.Kind)
		}
	}
	if maxInflight > 3 {
		t.Fatalf("并发上限 3 应生效, observed %d", maxInflight)
	}
}

// TestValidateDedupAttributionSorted 相同 values 只校验一次且归因取字典序
// 首台主机（b0 与 b1 同值缺 required，报错主机必须是 b0——不随 map 迭代
// 序漂移）。
func TestValidateDedupAttributionSorted(t *testing.T) {
	ch := &chart.Chart{Meta: chart.Meta{Required: []string{"data_dir"}}}
	err := Validate(ch, map[string]map[string]any{
		"b1": {"other": 1},
		"b0": {"other": 1},
		"a":  {"data_dir": "/x"},
	})
	if err == nil || !strings.Contains(err.Error(), "host b0:") {
		t.Fatalf("归因应为字典序首台 b0: %v", err)
	}
}

// TestValidateReportsUnserializable JSON 序列化失败的主机必须单独报错
// （CLI 旧版吞掉 marshal 错误，坏 values 互判相等而跳过校验），且按主机名
// 排序稳定。
func TestValidateReportsUnserializable(t *testing.T) {
	ch := &chart.Chart{Meta: chart.Meta{Required: []string{"data_dir"}}}
	err := Validate(ch, map[string]map[string]any{
		"z": {"nan": math.NaN()},
		"a": {"data_dir": "/ok"},
	})
	if err == nil {
		t.Fatal("序列化失败应报错（此前被吞）")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not JSON-serializable") ||
		!strings.Contains(msg, "host z:") {
		t.Fatalf("报错应点名主机 z: %v", err)
	}
}

// TestValidatePass 同值多主机只校验一次、合法 values 通过。
func TestValidatePass(t *testing.T) {
	ch := &chart.Chart{Meta: chart.Meta{Required: []string{"data_dir"}}}
	if err := Validate(ch, map[string]map[string]any{
		"h1": {"data_dir": "/x"}, "h2": {"data_dir": "/x"},
	}); err != nil {
		t.Fatalf("合法 values 应通过: %v", err)
	}
}

// TestFirstValues 代表性 values 取字典序首台主机（确定性），空集返回 nil。
func TestFirstValues(t *testing.T) {
	in := map[string]map[string]any{"zeta": {"v": 1}, "alpha": {"v": 2}}
	if got := FirstValues(in); got["v"] != 2 {
		t.Fatalf("应取字典序首台 alpha: %v", got)
	}
	if got := FirstValues(map[string]map[string]any{}); got != nil {
		t.Fatalf("空集应返回 nil: %v", got)
	}
}

// TestPerHostTimeout 单主机 ctx 超时应中断挂起的 Exec（console 侧 30s
// 口径在共享包内生效；fake.Exec 尊重 ctx 取消）。
func TestPerHostTimeout(t *testing.T) {
	block := make(chan struct{})
	conns := map[string]*fake.Fake{
		"slow": {ExecFn: func(conn.ExecRequest) (conn.ExecResult, error) {
			<-block // 挂起至放行（ctx 先到期则 fake 直接返回 ctx 错误）
			return conn.ExecResult{Code: 0, Stdout: "__MISSING__"}, nil
		}},
	}
	results := Read(context.Background(), "/m", []*model.Host{host("slow")},
		fakeDial(conns, new(int32)), Options{Concurrency: 1, PerHostTimeout: 30 * time.Millisecond})
	if results[0].Kind != KindFailed || !errors.Is(results[0].Err, context.DeadlineExceeded) {
		t.Fatalf("超时应归 failed 且成因为 DeadlineExceeded: %+v", results[0])
	}
	close(block) // 放行 ExecFn goroutine，避免泄漏
}
