package playbook

import "testing"

// 对账（键集合）：PlayFieldSections 的文档键必须与 playKeys（解析器认的
// play 级键）完全一致——文档缺键或多键都点名，防止文档与解析器漂移。
func TestPlayFieldsReconcile(t *testing.T) {
	documented := map[string]bool{}
	for _, sec := range PlayFieldSections() {
		for _, f := range sec.Fields {
			if documented[f.Name] {
				t.Fatalf("字段 %q 在多个分组重复出现", f.Name)
			}
			documented[f.Name] = true
		}
	}
	for k := range playKeys {
		if !documented[k] {
			t.Errorf("play 键 %q 缺少文档（playdoc.go 补充后才能对账通过）", k)
		}
	}
	for k := range documented {
		if !playKeys[k] {
			t.Errorf("文档字段 %q 不在 playKeys 中（解析器不认识，文档在捏造）", k)
		}
	}
}
