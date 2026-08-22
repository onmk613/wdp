package playbook

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"wdp/internal/i18n"
	"wdp/internal/model"
)

// YAML 值到结构体字段的类型转换与 schema 辅助（strategy/serial 解析）。

func toStringMap(v any) (map[string]string, error) {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]string, len(x))
		for k, val := range x {
			out[k] = fmt.Sprint(val)
		}
		return out, nil
	case map[string]string:
		return x, nil
	default:
		return nil, fmt.Errorf(i18n.T("expected map type, got %T", "需要 map 类型，实际 %T"), v)
	}
}

// toAnyMap 转换为 map[string]any。
func toAnyMap(v any) (map[string]any, error) {
	switch x := v.(type) {
	case map[string]any:
		return x, nil
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = val
		}
		return out, nil
	default:
		return nil, fmt.Errorf(i18n.T("expected map type, got %T", "需要 map 类型，实际 %T"), v)
	}
}

func toList(v any, what string) ([]any, error) {
	l, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf(i18n.T("%s must be a list, got %T", "%s 必须是列表，实际 %T"), what, v)
	}
	return l, nil
}

func toAnyList(v any, what string) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return x, nil
	case string:
		// 单个字符串项
		return []any{x}, nil
	default:
		return nil, fmt.Errorf(i18n.T("%s must be a list, got %T", "%s 必须是列表，实际 %T"), what, v)
	}
}

// parseSerial 解析 serial 表达式：整数 5、百分比 "10%"、或二者逗号列表 "5,10,20"
// （逐批尺寸，最后一个尺寸对剩余主机重复使用）。非法值报错而非静默忽略。
func parseSerial(v any) (string, error) {
	var tokens []string
	switch x := v.(type) {
	case int, float64:
		tokens = []string{fmt.Sprint(x)}
	case string:
		for _, t := range strings.Split(x, ",") {
			tokens = append(tokens, strings.TrimSpace(t))
		}
	case []any:
		for _, it := range x {
			tokens = append(tokens, strings.TrimSpace(fmt.Sprint(it)))
		}
	default:
		return "", fmt.Errorf(i18n.T("expected an integer, \"N%%\", or a list of them, got %T", "需要整数、\"N%%\" 或二者的列表，实际 %T"), v)
	}
	for _, t := range tokens {
		if t == "" {
			return "", errors.New(i18n.T("empty batch expression present", "存在空分批表达式"))
		}
		body := strings.TrimSuffix(t, "%")
		if body == "" || strings.Trim(body, "0123456789") != "" {
			return "", fmt.Errorf(i18n.T("cannot parse batch expression %q (use: 5 / \"10%%\" / \"5,10,20\")", "无法解析分批表达式 %q（可用: 5 / \"10%%\" / \"5,10,20\"）"), t)
		}
	}
	return strings.Join(tokens, ","), nil
}

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// parseStrategy 解析 play 级 strategy 配置。
func parseStrategy(v any) (*model.Strategy, error) {
	sm, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New(i18n.T("must be a map (type/batch/gate/auto_rollback)", "必须是 map（type/batch/gate/auto_rollback）"))
	}
	st := &model.Strategy{Type: "rolling"}
	if t, ok := sm["type"]; ok {
		st.Type = fmt.Sprint(t)
	}
	switch st.Type {
	case "linear", "rolling", "canary":
	default:
		return nil, fmt.Errorf(i18n.T("unsupported type %q (options: linear/rolling/canary)", "不支持的 type %q（可选: linear/rolling/canary）"), st.Type)
	}
	if b, ok := sm["batch"]; ok {
		st.Batch = fmt.Sprint(b)
	}
	if ar, ok := sm["auto_rollback"]; ok {
		b, err := model.ParseBool(ar)
		if err != nil {
			return nil, fmt.Errorf("auto_rollback: %w", err)
		}
		st.AutoRollback = b
	}
	if g, ok := sm["gate"]; ok {
		gm, ok := g.(map[string]any)
		if !ok {
			return nil, errors.New(i18n.T("gate must be a map (shell/until/retries/delay)", "gate 必须是 map（shell/until/retries/delay）"))
		}
		gate := &model.Task{Name: "health-gate", Module: "shell"}
		if s, ok := gm["shell"]; ok {
			gate.FreeForm = fmt.Sprint(s)
		}
		if gate.FreeForm == "" {
			return nil, errors.New(i18n.T("gate requires shell (health check command)", "gate 需要 shell（健康检查命令）"))
		}
		if u, ok := gm["until"]; ok {
			gate.Until = fmt.Sprint(u)
		}
		if gate.Until == "" {
			// 缺省健康门：命令退出码为 0
			gate.Until = `{{ if eq .result.rc 0 }}ok{{ end }}`
		}
		if r, ok := gm["retries"]; ok {
			gate.Retries = toInt(r)
		}
		if gate.Retries <= 0 {
			gate.Retries = 3
		}
		if d, ok := gm["delay"]; ok {
			gate.DelaySec = toInt(d)
		}
		st.Gate = gate
	}
	return st, nil
}
