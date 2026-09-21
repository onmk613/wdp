package agent

// 自治执行请求参数的校验与展示辅助。
//
// run_id 由控制端生成并直接拼进运行目录路径（<runs_root>/<run_id>），
// 未校验时 "../.." 一类取值可让以 root 运行的 agent 在任意目录建目录、
// 写 plan.json/journal.ndjson。plan_id 是内容寻址摘要，理论上总是 64 位
// 十六进制，但请求体来自网络——展示时截断必须防越界。

import (
	"fmt"
	"regexp"
)

// runIDRe 限定 run_id 字符集与长度：字母/数字开头，随后字母数字与 . _ -，
// 总长 1..64。句点允许但由 filepath.Join + 后续路径校验共同约束。
var runIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// validateRunID 校验 run_id 合法性（不合法即拒绝，不做任何"清洗"）。
func validateRunID(id string) error {
	if !runIDRe.MatchString(id) {
		return fmt.Errorf("run_id %q is invalid (allowed: 1-64 chars, letters/digits/'.'/'_'/'-', must start alphanumeric)", id)
	}
	return nil
}

// shortID 返回摘要前缀（错误消息/日志用）。短于 12 位时原样返回——
// 请求体可控，直接切片会 panic。
func shortID(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
