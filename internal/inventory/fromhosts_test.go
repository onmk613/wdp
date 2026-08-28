package inventory

// FromHosts（--hosts 内联主机模式的最小 inventory）测试。

import (
	"testing"

	"wdp/internal/model"
)

func TestFromHosts(t *testing.T) {
	inv := FromHosts([]*model.Host{
		{Name: "10.8.2.101", Address: "10.8.2.101"},
		{Name: "10.8.2.102", Address: "10.8.2.102"},
		{Name: "10.8.2.102", Address: "dup"}, // 去重
		nil,
	})
	if len(inv.Hosts) != 2 {
		t.Fatalf("主机数: %d", len(inv.Hosts))
	}
	// Select("all") 覆盖全部内联主机
	hosts, err := inv.Select("all")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("Select all: %d", len(hosts))
	}
	// 内置变量数据源就绪：groups/hosts 元信息
	if g := inv.GroupsMap()["all"]; len(g) != 2 {
		t.Fatalf("groups.all: %v", g)
	}
	if inv.HostsMeta()["10.8.2.102"] == nil {
		t.Fatal("hosts 元信息缺失")
	}
	// 每台主机变量域带 group_names（内置变量注入口径）
	if gn := inv.Hosts[0].Vars["group_names"].([]string); len(gn) != 1 || gn[0] != "all" {
		t.Fatalf("group_names: %v", gn)
	}
	// 命名组不存在（内联模式语义）
	if _, err := inv.Select("webservers"); err == nil {
		t.Fatal("命名组不应存在")
	}
}
