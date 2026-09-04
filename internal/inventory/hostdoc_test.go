package inventory

import (
	"reflect"
	"testing"

	"wdp/internal/model"
)

// 对账测试（基线部分）：内置 hostKeys 白名单的每个键都必须出现在
// HostFieldSections 文档里——新增基线键漏写文档直接红。
// 连接包注册的专属键（agent/push 组）由 cli 包的集成对账测试锁定
// （那里 blank import 了全部连接驱动，HostKeys() 才是全集）。
func TestHostFieldsReconcile(t *testing.T) {
	documented := map[string]bool{}
	for _, sec := range HostFieldSections() {
		for _, f := range sec.Fields {
			if documented[f.Name] {
				t.Fatalf("字段 %q 在多个分组重复出现", f.Name)
			}
			documented[f.Name] = true
		}
	}
	for k := range hostKeys {
		if !documented[k] {
			t.Errorf("主机键 %q 缺少文档（hostdoc.go 补充后才能对账通过）", k)
		}
	}
}

// hostKeySentinels 每个文档键的哨兵值（apply 从零值 Host 起步，真值即可
// 与零值区分；端口等取非默认值以证明确实写入）。
func hostKeySentinels() map[string]any {
	return map[string]any{
		"host": "10.9.9.9", "port": 2222, "conn": "ssh", "user": "zz",
		"password": "zz", "password_env": "zz",
		"key_path": "/zz", "key_passphrase": "zz", "key_passphrase_env": "zz",
		"host_key_check": true, "known_hosts": "/zz", "connect_timeout": 77,
		"become_password": "zz", "become_password_env": "zz",
		"agent_url": "http://zz:1", "agent_port": 7603, "tls": true,
		"ca_file": "/zz", "cert_file": "/zz", "key_file": "/zz",
		"insecure_skip_verify": true, "tls_skip_host_verify": true,
		"tls_server_name": "zz", "binary_path": "/zz", "keep_agent": true,
	}
}

// TestHostFieldsPopulateStruct 行为对账：每个文档键经 hostKeyApplier 真实
// 写入一次，反射断言 GoField 声称的 model.Host 字段被填充——文档键忘在
// apply switch 补解析（键被白名单吞掉、值静默丢弃）、或落点字段与 GoField
// 不符，此处点名报错。这是文档与 model.Host 的机械关联。
func TestHostFieldsPopulateStruct(t *testing.T) {
	sentinels := hostKeySentinels()
	for _, sec := range HostFieldSections() {
		for _, f := range sec.Fields {
			sv, ok := sentinels[f.Name]
			if !ok {
				t.Errorf("键 %q 缺少哨兵用例（hostdoc_test.go 的 hostKeySentinels 补充）", f.Name)
				continue
			}
			h := &model.Host{}
			ap := &hostKeyApplier{h: h, connectTimeoutDef: 10}
			if err := ap.apply(f.Name, sv); err != nil {
				t.Errorf("键 %q: apply 失败: %v", f.Name, err)
				continue
			}
			v := reflect.Indirect(reflect.ValueOf(h)).FieldByName(f.GoField)
			if !v.IsValid() {
				t.Errorf("键 %q: GoField %q 在 model.Host 中不存在（字段改名？同步 hostdoc.go）", f.Name, f.GoField)
				continue
			}
			if v.IsZero() {
				t.Errorf("键 %q: apply 后 GoField %q 仍为零值（忘在 hostKeyApplier.apply 补 case，或落点字段不对）", f.Name, f.GoField)
			}
		}
	}
}
