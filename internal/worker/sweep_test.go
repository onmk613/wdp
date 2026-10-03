package worker

// 周期扫描（Prober/Monitor）主机读取的契约测试：AllHosts 一次查询覆盖
// 全部主机，连接字段与台账一致，池/组多值列不携带。

import (
	"path/filepath"
	"testing"

	"wdp/internal/store"
)

// TestAllHostsScanFields 字段口径：address/agent_port/allow_plaintext 与
// 台账一致（探活与采样回调只消费这几列），标量列齐全，Pools/Groups 为
// nil——需要多值归属的调用方走 ListHosts/GetHost，不得依赖 AllHosts。
func TestAllHostsScanFields(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "wdp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateHost(&store.Host{
		Name: "a", Address: "10.0.0.1", AgentPort: 7603,
		Pools: []string{"p1"}, Labels: `{"env":"prod"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateHost(&store.Host{Name: "b", Address: "10.0.0.2", AllowPlaintext: true}); err != nil {
		t.Fatal(err)
	}
	hosts, err := st.AllHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("应一次取回全部 2 台: %+v", hosts)
	}
	byName := map[string]*store.Host{}
	for _, h := range hosts {
		byName[h.Name] = h
	}
	a, b := byName["a"], byName["b"]
	if a == nil || b == nil {
		t.Fatalf("主机缺失: %+v", hosts)
	}
	if a.Address != "10.0.0.1" || a.AgentPort != 7603 || a.Labels != `{"env":"prod"}` || a.Status != "unknown" {
		t.Fatalf("a 的标量列应与台账一致: %+v", a)
	}
	if b.AgentPort != 7602 || !b.AllowPlaintext || a.AllowPlaintext {
		t.Fatalf("端口缺省/明文标志应正确回填: a=%+v b=%+v", a, b)
	}
	for _, h := range hosts {
		if h.Pools != nil || h.Groups != nil {
			t.Fatalf("AllHosts 不携带池/组多值列: %+v", h)
		}
	}
}
