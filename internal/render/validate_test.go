package render

// 模板静态校验测试：裸变量引用（Ansible 习语）必须在 parse 阶段被拦截，
// 合法写法（前导点、白名单函数）不得误报。

import (
	"strings"
	"testing"

	"wdp/internal/model"
)

func TestValidateTemplateBareIdentCaught(t *testing.T) {
	eng := DefaultEngine()
	// 裸标识符：parse 期即报 function not defined（用户现场的最小复现）
	err := eng.ValidateTemplate("docker exec -i {{ doris_container_id.stdout }} bash -c \"rm -rf core.*\"")
	if err == nil {
		t.Fatal("裸变量引用应被拦截")
	}
	if !strings.Contains(err.Error(), `function "doris_container_id" not defined`) {
		t.Fatalf("错误应指明未定义标识符: %v", err)
	}
	if !strings.Contains(err.Error(), "leading dot") {
		t.Fatalf("错误应附前导点修正提示: %v", err)
	}
}

func TestValidateTemplateValidForms(t *testing.T) {
	eng := DefaultEngine()
	ok := []string{
		"",           // 无模板
		"plain text", // 无定界符
		"docker exec -i {{ .doris_container_id.stdout }} bash", // 前导点
		"{{ upper .name }}",            // 白名单函数
		`{{ if eq .a "x" }}y{{ end }}`, // 控制结构
		"{{ .a }} {{ .b.c }}",          // 多引用
	}
	for _, tpl := range ok {
		if err := eng.ValidateTemplate(tpl); err != nil {
			t.Fatalf("%q 不应报错: %v", tpl, err)
		}
	}
}

func TestValidateTemplateSyntaxError(t *testing.T) {
	if err := DefaultEngine().ValidateTemplate("{{ .x"); err == nil {
		t.Fatal("语法错误应被拦截")
	}
}

func TestRenderBareIdentHint(t *testing.T) {
	// 运行时渲染错误同样附提示（连接用户现场报错的改进）
	_, err := DefaultEngine().Render("{{ var.stdout }}", map[string]any{"var": map[string]any{"stdout": "x"}})
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "leading dot") {
		t.Fatalf("缺前导点提示: %v", err)
	}
}

func TestValidateTaskTemplates(t *testing.T) {
	eng := DefaultEngine()
	task := &model.Task{
		Module:   "shell",
		FreeForm: "echo {{ var }}", // 裸变量 → free-form 报错
		Args: map[string]any{
			"cmd":   "x {{ .ok }}",
			"bad":   "y {{ bad_ref }}", // 字符串参数校验
			"count": 3,                 // 非字符串不渲染，不校验
		},
		When:        []string{"{{ .flag }}"},
		ChangedWhen: "{{ .c }}",
		FailedWhen:  "{{ f }}", // 裸变量 → failed_when 报错
		Until:       "{{ .result.rc }}",
		Environment: map[string]string{"PATH": "{{ p }}"}, // 裸变量 → env 报错
	}
	errs := eng.ValidateTaskTemplates(task)
	hasField := func(f string) bool {
		for _, e := range errs {
			if strings.HasPrefix(e, f+":") {
				return true
			}
		}
		return false
	}
	for _, field := range []string{"free-form", "args.bad", "failed_when", "env.PATH"} {
		if !hasField(field) {
			t.Fatalf("缺 %s 的报错: %v", field, errs)
		}
	}
	for _, field := range []string{"args.cmd", "when", "changed_when", "until"} {
		if hasField(field) {
			t.Fatalf("%s 不应报错: %v", field, errs)
		}
	}
}
