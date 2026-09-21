package module

import (
	"strings"
	"testing"
)

func TestDebugModule(t *testing.T) {
	rc := &RunContext{Vars: map[string]any{
		"doris": map[string]any{"stdout": "abc123\n", "rc": 0},
	}}
	m := &DebugModule{}
	r := m.Run(rc, map[string]any{"var": "doris.stdout"}, "")
	if r.Failed || !strings.Contains(r.Msg, "abc123") {
		t.Fatalf("var 解析失败: %+v", r)
	}
	r = m.Run(rc, map[string]any{"msg": "id={{ .doris.stdout }}"}, "")
	if r.Failed || !strings.Contains(r.Msg, "id=abc123") {
		t.Fatalf("msg 渲染失败: %+v", r)
	}
	if r := m.Run(rc, map[string]any{"var": "nope.x"}, ""); !r.Failed {
		t.Fatal("未知变量应报错")
	}
	if r := m.Run(rc, nil, ""); !r.Failed {
		t.Fatal("缺参数应报错")
	}
}
