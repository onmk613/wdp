package playbook

import (
	"fmt"
	"reflect"
	"testing"

	"wdp/internal/model"
)

// 对账（键集合）：taskKeys 由 TaskFieldSections 派生（单一来源），此处
// 校验派生口径——文档不得捏造未派生进白名单的键，模块键写法显式排除。
func TestTaskFieldsReconcile(t *testing.T) {
	documented := map[string]bool{}
	for _, sec := range TaskFieldSections() {
		for _, f := range sec.Fields {
			if documented[f.Name] {
				t.Fatalf("字段 %q 在多个分组重复出现", f.Name)
			}
			documented[f.Name] = true
		}
	}
	for name := range documented {
		if !taskKeys[name] && !moduleKeyWrites[name] {
			t.Errorf("文档字段 %q 未派生进 taskKeys，也不是模块键写法", name)
		}
	}
	for k := range taskKeys {
		if !documented[k] {
			t.Errorf("控制键 %q 缺少文档（taskdoc.go 补充后才能对账通过）", k)
		}
	}
}

// taskKeySentinel 每个控制键的哨兵值与构造基座：特殊键（chart 引用组、
// 模块键写法、args 合并语义）需要不同的任务形态才能落进声称的字段。
type taskKeySentinel struct {
	value  any                     // 该键的哨兵值
	module string                  // 基座模块写法（缺省 shell 简写）
	assert func(*model.Task) error // 缺省用 GoField 反射非零断言
}

// oneTask 哨兵用的单任务列表（block/rescue/always 组值）。
var oneTask = []any{map[string]any{"shell": "x"}}

func taskSentinels() map[string]taskKeySentinel {
	return map[string]taskKeySentinel{
		"name": {value: "zz"}, "when": {value: []any{"true"}},
		"loop": {value: []any{"a"}}, "with_items": {value: []any{"a"}},
		"loop_control": {value: map[string]any{"loop_var": "zz"}},
		"until":        {value: "zz"}, "retries": {value: 7}, "delay": {value: 7}, "timeout": {value: 7},
		"environment": {value: map[string]any{"K": "V"}},
		"become":      {value: true}, "become_user": {value: "zz"},
		"delegate_to": {value: "zz"}, "run_once": {value: true},
		"ignore_errors": {value: true}, "hook": {value: "post_deploy"},
		"register": {value: "zz"}, "notify": {value: []any{"n"}}, "tags": {value: []any{"t"}},
		"changed_when": {value: "zz"}, "failed_when": {value: "zz"},
		"output": {value: "none"}, "no_log": {value: true},
		"block": {value: oneTask},
		// rescue/always 必须与 block 同现（parseTask 校验）
		"rescue": {module: "block", value: oneTask},
		"always": {module: "block", value: oneTask},
		// chart 引用组：vars/tasks_from 只在 chart 引用任务上合法
		"chart":      {module: "", value: "sub"},
		"include":    {module: "", value: "f.yaml"},
		"vars":       {module: "chart", value: map[string]any{"k": "v"}},
		"tasks_from": {module: "chart", value: "install"},
		// args 与模块参数合并：断言哨兵键确实并入 Args（反射非零会被
		// 模块自身参数淹没，须看内容）
		"args": {module: "file-map", value: map[string]any{"sentinel_arg": "v"},
			assert: func(t2 *model.Task) error {
				if _, ok := t2.Args["sentinel_arg"]; !ok {
					return fmt.Errorf("args 哨兵键未并入 Args: %v", t2.Args)
				}
				return nil
			}},
	}
}

// TestTaskKeysFrozen 冻结控制键全集。taskKeys 派生自文档表——误删一行
// FieldDoc 会静默收窄白名单（该键被当模块名，任务解析报 "multiple
// modules"），此处对照冻结清单点名：增删键必须是有意的（文档 + 此处
// 同步改）。host 侧的等价防护由 cli 集成对账的反向断言承担
// （documented ⊆ HostKeys()）。
func TestTaskKeysFrozen(t *testing.T) {
	frozen := map[string]bool{
		"name": true, "when": true, "loop": true, "with_items": true,
		"register": true, "notify": true, "tags": true, "environment": true,
		"ignore_errors": true, "retries": true, "delay": true, "timeout": true,
		"become": true, "become_user": true,
		"changed_when": true, "failed_when": true, "args": true,
		"vars": true, "tasks_from": true, "until": true,
		"block": true, "rescue": true, "always": true,
		"output": true, "no_log": true,
		"delegate_to": true, "run_once": true, "loop_control": true, "hook": true,
	}
	if !reflect.DeepEqual(taskKeys, frozen) {
		t.Fatalf("taskKeys 与冻结清单不一致（有意增删键？同步此处与 taskdoc.go）:\n  got  %v\n  want %v", taskKeys, frozen)
	}
}

// TestTaskFieldsPopulateStruct 行为对账：每个文档键用哨兵值真实走一遍
// parseTask，反射断言 GoField 声称的结构体字段被填充——文档键忘写解析、
// 或解析落点与 GoField 不符，此处点名报错。这是文档与 model.Task 的
// 机械关联，替代人工保持两处一致。
func TestTaskFieldsPopulateStruct(t *testing.T) {
	sentinels := taskSentinels()
	for _, sec := range TaskFieldSections() {
		for _, f := range sec.Fields {
			st, ok := sentinels[f.Name]
			if !ok {
				t.Errorf("键 %q 缺少哨兵用例（taskdoc_test.go 的 taskSentinels 补充）", f.Name)
				continue
			}
			task, err := parseSentinelTask(st, f.Name)
			if err != nil {
				t.Errorf("键 %q: 解析失败: %v", f.Name, err)
				continue
			}
			if st.assert != nil {
				if aerr := st.assert(task); aerr != nil {
					t.Errorf("键 %q: %v", f.Name, aerr)
				}
				continue
			}
			v := reflect.Indirect(reflect.ValueOf(task)).FieldByName(f.GoField)
			if !v.IsValid() {
				t.Errorf("键 %q: GoField %q 在 model.Task 中不存在（字段改名？同步 taskdoc.go）", f.Name, f.GoField)
				continue
			}
			if v.IsZero() {
				t.Errorf("键 %q: 解析后 GoField %q 仍为零值（忘在 parseTask* 补解析，或落点字段不对）", f.Name, f.GoField)
			}
		}
	}
}

// parseSentinelTask 构造含哨兵键的最小任务并解析。基座模块写法：
// ""（shell 简写）、"chart"（子 chart 引用）、"file-map"（file 模块 map 值）。
func parseSentinelTask(st taskKeySentinel, key string) (*model.Task, error) {
	m := map[string]any{"name": "t"}
	switch st.module {
	case "chart", "": // 模块键写法（chart/include）不叠加坡座
		if moduleKeyWrites[key] {
			m[key] = st.value
			return parseTask(m, false)
		}
		if st.module == "chart" {
			m["chart"] = "sub"
		} else {
			m["shell"] = "x"
		}
	case "file-map":
		m["file"] = map[string]any{"path": "/x"}
	case "block":
		m["block"] = oneTask
	}
	m[key] = st.value
	return parseTask(m, false)
}
