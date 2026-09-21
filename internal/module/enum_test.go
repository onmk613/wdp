package module

import "testing"

// TestParamEnumsWellFormed 枚举字段对账：声明了 Enum 的参数——值非空、
// 无重复；Default 非空时必须在枚举内（缺省值漂移到值域外会被点名）。
// Enum 与解析白名单的一致性由"共用同一包级变量"从构造上保证
// （如 fileStates 同时被 parseFileArgs 与 Params() 引用），无需运行时对账。
func TestParamEnumsWellFormed(t *testing.T) {
	for _, name := range Names() {
		m, _ := Get(name)
		for _, p := range Usage(m) {
			if len(p.Enum) == 0 {
				continue
			}
			seen := map[string]bool{}
			for _, v := range p.Enum {
				if v == "" {
					t.Errorf("%s.%s: enum contains an empty value", name, p.Name)
					continue
				}
				if seen[v] {
					t.Errorf("%s.%s: enum duplicate value %q", name, p.Name, v)
				}
				seen[v] = true
			}
			if p.Default != "" && !seen[p.Default] {
				t.Errorf("%s.%s: default %q is not in enum %v", name, p.Name, p.Default, p.Enum)
			}
		}
	}
}

// TestStateParamsCarryEnums 解析期硬校验 state 值域的模块必须声明 Enum
// （值补全候选来自 /api/modules 的 ParamDoc.Enum，漏声明则编辑器无值
// 补全，白名单与文档退回两处维护）。新增走 parseState/内联校验的模块
// 时把名字加进这里。
func TestStateParamsCarryEnums(t *testing.T) {
	validated := map[string]string{
		"file":         "state",
		"service":      "state",
		"systemd_unit": "state",
		"package":      "state",
		"user":         "state",
		"group":        "state",
		"lineinfile":   "state",
		"wait_for":     "state",
	}
	for name, key := range validated {
		m, ok := Get(name)
		if !ok {
			t.Fatalf("%s: module not registered", name)
		}
		for _, p := range Usage(m) {
			if p.Name == key && len(p.Enum) == 0 {
				t.Errorf("%s.%s: parser validates a fixed value set but Params() declares no Enum", name, key)
			}
		}
	}
}
