package chart

// chart.yaml 字段表的对账测试：文档与 Meta/PhaseSpec 的 yaml tag 双向一致，
// 新增字段忘写文档（或文档捏造字段）即失败。

import (
	"reflect"
	"strings"
	"testing"
)

// yamlTagsOf 返回结构体的全部 yaml 键（跳过 "-"）。
func yamlTagsOf(v any) map[string]string {
	out := map[string]string{}
	t := reflect.TypeOf(v)
	for i := range t.NumField() {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		out[tag] = f.Name
	}
	return out
}

func TestChartFieldsReconcile(t *testing.T) {
	metaTags := yamlTagsOf(Meta{})
	documented := map[string]string{} // 文档键 → GoField
	phaseDocs := map[string]bool{}    // 相位属性表的文档键
	for _, sec := range ChartFieldSections() {
		isPhase := strings.HasPrefix(sec.Title, "相位属性")
		for _, f := range sec.Fields {
			if prev, dup := documented[f.Name]; dup {
				t.Fatalf("字段 %q 在多个分组重复出现（前一处 GoField=%s）", f.Name, prev)
			}
			documented[f.Name] = f.GoField
			if isPhase {
				phaseDocs[f.Name] = true
			}
		}
	}
	// 文档 → Meta：非相位表的每个字段都必须对应真实 yaml tag 与结构体字段
	for name, goField := range documented {
		if phaseDocs[name] {
			continue // 相位属性属于 PhaseSpec，单独对账
		}
		want, ok := metaTags[name]
		if !ok {
			t.Errorf("文档字段 %q 在 chart.Meta 中没有对应 yaml tag", name)
			continue
		}
		if goField != want {
			t.Errorf("文档字段 %q 的 GoField=%q，实际结构体字段是 %q", name, goField, want)
		}
	}
	// Meta → 文档：每个 yaml 键都必须有文档
	for tag := range metaTags {
		if _, ok := documented[tag]; !ok {
			t.Errorf("chart.Meta 的 yaml 键 %q 缺少文档（chartdoc.go 补充后才能对账通过）", tag)
		}
	}
	// 相位属性表与 PhaseSpec 双向对账
	phaseTags := yamlTagsOf(PhaseSpec{})
	for tag := range phaseTags {
		if !phaseDocs[tag] {
			t.Errorf("PhaseSpec 的 yaml 键 %q 缺少文档", tag)
		}
	}
	for tag := range phaseDocs {
		if _, ok := phaseTags[tag]; !ok {
			t.Errorf("文档中的相位属性 %q 在 PhaseSpec 中不存在", tag)
		}
	}
	for _, name := range PhaseFieldNames() {
		if !phaseDocs[name] {
			t.Errorf("PhaseFieldNames 列出的 %q 缺少文档", name)
		}
	}
}
