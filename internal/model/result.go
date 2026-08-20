package model

import (
	"strconv"
	"strings"
)

// ApplyOutputSpec 按任务级 output 展示控制表达式裁剪文本。
// 可用：full（原文）/ none（清空）/ oneline（首行）/ head=N / tail=N。
// 只影响展示（reporter），不影响 register 数据。
func ApplyOutputSpec(spec, s string) string {
	switch spec {
	case "", "full":
		return s
	case "none":
		return ""
	case "oneline":
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			return strings.TrimRight(s[:i], "\r")
		}
		return s
	}
	if rest, ok := strings.CutPrefix(spec, "head="); ok {
		if n, err := strconv.Atoi(rest); err == nil && n >= 0 {
			lines := strings.Split(s, "\n")
			if len(lines) > n {
				return strings.Join(lines[:n], "\n") + "\n…"
			}
		}
		return s
	}
	if rest, ok := strings.CutPrefix(spec, "tail="); ok {
		if n, err := strconv.Atoi(rest); err == nil && n >= 0 {
			lines := strings.Split(s, "\n")
			if len(lines) > n {
				return "…\n" + strings.Join(lines[len(lines)-n:], "\n")
			}
		}
		return s
	}
	return s
}

// TaskResult 是一次任务（或循环中一项）在某主机上的执行结果。
type TaskResult struct {
	Host   string `json:"host"`
	Task   string `json:"task"`
	Module string `json:"module"`

	Skipped     bool           `json:"skipped"`        // when 不满足
	SkipReason  string         `json:"skip_reason"`    // 跳过原因（when 表达式）
	Failed      bool           `json:"failed"`         // 执行失败（模块判定或异常）
	Unreachable bool           `json:"unreachable"`    // 连接失败
	Changed     bool           `json:"changed"`        // 是否变更了目标机状态
	Msg         string         `json:"msg"`            // 人读消息（失败原因或摘要）
	Stdout      string         `json:"stdout"`         // 命令标准输出
	Stderr      string         `json:"stderr"`         // 命令标准错误
	Rc          int            `json:"rc"`             // 命令退出码
	Data        map[string]any `json:"data,omitempty"` // 结构化数据（register 时并入）

	// 展示与编排扩展
	Output     string        `json:"output,omitempty"`      // 展示控制：full|none|oneline|head=N|tail=N（只控展示不控数据）
	NoLog      bool          `json:"no_log,omitempty"`      // 敏感任务：stdout/stderr/msg/diff 在所有输出面（含 JSON 报告）遮蔽
	Item       string        `json:"item,omitempty"`        // loop 项标签（逐项展示用）
	Items      []*TaskResult `json:"items,omitempty"`       // loop 逐项结果（JSON 记录与逐项展示）
	DelegateTo string        `json:"delegate_to,omitempty"` // 委托执行的目标主机
	Diff       string        `json:"diff,omitempty"`        // --diff 内容级差异（unified diff 或属性前后对照）

	ElapsedMs int64 `json:"elapsed_ms"` // 耗时毫秒
}

// Stats 是按主机聚合的执行统计。
type Stats struct {
	Ok          int `json:"ok"`
	Changed     int `json:"changed"`
	Failed      int `json:"failed"`
	Unreachable int `json:"unreachable"`
	Skipped     int `json:"skipped"`
	Ignored     int `json:"ignored"` // ignore_errors 吞掉的失败
}
