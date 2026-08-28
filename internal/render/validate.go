package render

// 模板静态校验（parse-only）：lint 在不执行任务的前提下提前发现
// "运行时才炸"的模板错误。语法错误与未注册的函数引用在 parse 阶段即可
// 判定且不依赖运行时变量——最常见的是 Ansible 风格的裸变量引用
// {{ var.stdout }}：Go template 语义下裸标识符是函数调用，报
// function "var" not defined（需写成 {{ .var.stdout }}）。

import (
	"fmt"
	"strings"

	"wdp/internal/model"
)

// ValidateTemplate 静态校验单个模板字符串（parse-only，不执行）：
// 变量是否存在不影响 parse，因此无需运行时变量域。
func (e *Engine) ValidateTemplate(tpl string) error {
	if !strings.Contains(tpl, "{{") {
		return nil
	}
	clone, err := e.base.Clone()
	if err != nil {
		return err
	}
	if _, err := clone.New("w").Parse(tpl); err != nil {
		return fmt.Errorf("template parse failed %q: %w%s", tpl, err, bareIdentHint(err))
	}
	return nil
}

// ValidateTaskTemplates 校验任务中会被渲染的全部字段，返回错误消息列表
// （空 = 通过）。字段集合对应 executor 的渲染面（exec/task/expand.go），
// executor 新增渲染字段时需同步此处。
func (e *Engine) ValidateTaskTemplates(t *model.Task) []string {
	var errs []string
	add := func(field, tpl string) {
		if err := e.ValidateTemplate(tpl); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", field, err))
		}
	}
	if t.FreeForm != "" {
		add("free-form", t.FreeForm)
	}
	// Args/Loop 经 RenderValue 递归渲染：只有字符串是模板
	for k, v := range t.Args {
		if s, ok := v.(string); ok {
			add("args."+k, s)
		}
	}
	for _, item := range t.Loop {
		if s, ok := item.(string); ok {
			add("loop", s)
		}
	}
	for _, w := range t.When {
		add("when", w)
	}
	for _, w := range []struct{ field, tpl string }{
		{"changed_when", t.ChangedWhen},
		{"failed_when", t.FailedWhen},
		{"until", t.Until},
		{"delegate_to", t.DelegateTo},
	} {
		if w.tpl != "" {
			add(w.field, w.tpl)
		}
	}
	errs = append(errs, e.validateEnv(t.Environment)...)
	return errs
}

// ValidateEnv 校验环境变量值（play 级与任务级同构）。
func (e *Engine) validateEnv(env map[string]string) []string {
	var errs []string
	for k, v := range env {
		if err := e.ValidateTemplate(v); err != nil {
			errs = append(errs, fmt.Sprintf("env.%s: %v", k, err))
		}
	}
	return errs
}

// ValidateEnv 校验环境变量映射（lint 对 play 级 environment 使用）。
func (e *Engine) ValidateEnv(env map[string]string) []string {
	return e.validateEnv(env)
}
