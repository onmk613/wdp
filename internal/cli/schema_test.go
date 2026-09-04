package cli

// `wdp schema` 的测试：命令注册/补全、渲染冒烟，以及跨包集成对账——
// 本包 blank import 了全部连接驱动（common.go），inventory.HostKeys()
// 在此才是全集（基线 + agent/push 注册键），与文档表对账防漂移。

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/playbook"
)

// schemaSections 取域的字段表。
func schemaSections(domain string) []model.FieldSection {
	if domain == "host" {
		return inventory.HostFieldSections()
	}
	return playbook.TaskFieldSections()
}

// TestSchemaRegistered 命令注册且归属 Package 组。
func TestSchemaRegistered(t *testing.T) {
	root := NewRootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "schema" {
			if c.GroupID != "chart" {
				t.Fatalf("schema 分组 %q，期望 chart", c.GroupID)
			}
			return
		}
	}
	t.Fatal("缺少顶层命令 schema")
}

// TestSchemaCompletion 补全候选 host/task 在前、带描述、按前缀过滤。
func TestSchemaCompletion(t *testing.T) {
	cmd := newSchemaCmd()
	if cmd.ValidArgsFunction == nil {
		t.Fatal("schema 未配置 ValidArgsFunction")
	}
	got, dir := cmd.ValidArgsFunction(cmd, nil, "")
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("directive = %v，期望 NoFileComp", dir)
	}
	if len(got) != 2 || !strings.HasPrefix(got[0], "host\t") || !strings.HasPrefix(got[1], "task\t") {
		t.Fatalf("候选应为 host/task（host 在前）: %v", got)
	}
	if got, _ := cmd.ValidArgsFunction(cmd, nil, "ho"); len(got) != 1 || !strings.HasPrefix(got[0], "host") {
		t.Fatalf("前缀 ho 应只剩 host: %v", got)
	}
	if got, _ := cmd.ValidArgsFunction(cmd, []string{"host"}, ""); got != nil {
		t.Fatalf("参数已齐应返回空: %v", got)
	}
}

// TestSchemaRender 渲染冒烟：域表含分组标题/字段行/示例，未知域报错。
func TestSchemaRender(t *testing.T) {
	for _, domain := range []string{"host", "task"} {
		var buf bytes.Buffer
		printFieldSections(&buf, domain, schemaSections(domain))
		s := buf.String()
		if !strings.Contains(s, "== ") {
			t.Fatalf("%s 输出缺少分组标题:\n%s", domain, s)
		}
		if !strings.Contains(s, "example:") {
			t.Fatalf("%s 输出缺少示例片段:\n%s", domain, s)
		}
	}
}

// TestSchemaIntegrationReconcileHostKeys 集成对账：连接驱动全部加载后，
// HostKeys() 全集（基线 + agent/push 注册键）的每个键都必须有文档。
func TestSchemaIntegrationReconcileHostKeys(t *testing.T) {
	documented := map[string]bool{}
	for _, sec := range inventory.HostFieldSections() {
		for _, f := range sec.Fields {
			documented[f.Name] = true
		}
	}
	for _, k := range inventory.HostKeys() {
		if !documented[k] {
			t.Errorf("已注册主机键 %q 缺少文档（hostdoc.go 补充后才能对账通过）", k)
		}
	}
	// 反向：文档不得捏造未注册的键
	for name := range documented {
		found := false
		for _, k := range inventory.HostKeys() {
			if k == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("文档字段 %q 不是已注册的主机键", name)
		}
	}
}
