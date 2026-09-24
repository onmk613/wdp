package executor

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"wdp/internal/model"
	"wdp/internal/module"
)

// 任务/模块结果到变量域的数据映射与结果工具。

func resultData(r *model.TaskResult) map[string]any {
	return map[string]any{
		"changed": r.Changed,
		"failed":  r.Failed,
		"rc":      r.Rc,
		"stdout":  r.Stdout,
		"stderr":  r.Stderr,
		"msg":     r.Msg,
		"skipped": r.Skipped,
	}
}

// moduleData 构造 until/changed_when 判断用的模块结果域。
func moduleData(mr *module.Result) map[string]any {
	if mr == nil {
		return map[string]any{}
	}
	return map[string]any{
		"changed": mr.Changed,
		"failed":  mr.Failed,
		"rc":      mr.Rc,
		"stdout":  mr.Stdout,
		"stderr":  mr.Stderr,
		"msg":     mr.Msg,
	}
}

func cloneRes(r *model.TaskResult) *model.TaskResult {
	return new(*r)
}

// maxOutLen 是单任务捕获输出的上限（防 cat 大文件撑爆内存）。
const maxOutLen = 1 << 20

// truncateOut 截断超长输出并标注。按字节截断后退到 UTF-8 边界：
// 截断点落在多字节字符中间会产生 mojibake。
func truncateOut(s string) string {
	if len(s) <= maxOutLen {
		return s
	}
	return truncateAtBoundary(s, maxOutLen) + fmt.Sprintf("\n…[wdp] output truncated (%d bytes)", len(s))
}

// truncateAtBoundary 按字节截断并回退到完整 UTF-8 序列边界（最多回退
// utf8.UTFMax-1 字节；非法序列原样保留——截断宽容优于报错）。
func truncateAtBoundary(s string, max int) string {
	if max >= len(s) {
		return s
	}
	cut := max
	for i := 0; i < utf8.UTFMax-1 && cut > 0; i++ {
		if b := s[cut-1]; b&0xC0 != 0x80 { // 非续字节：cut 已在序列边界
			break
		}
		cut--
	}
	return s[:cut]
}

// aggregateTruncMark 是 loop/子 chart 聚合输出触顶后的截断标记。定长
// 预留使追加内容 + 标记的总量恒 ≤ maxOutLen；以 HasSuffix 判重（O(1)，
// 避免万级 item 下反复全文扫描）。
const aggregateTruncMark = "\n…[wdp] aggregate output truncated at 1MiB"

// appendCapped 带总量上限的输出追加：单项输出已有 maxOutLen 截断，但
// loop/子 chart 聚合逐项 `+=` 不封顶，万级 item × 1MiB 仍可撑爆内存。
// 超出后丢弃后续内容并（仅首次）追加截断标记。
func appendCapped(dst, add string) string {
	room := maxOutLen - len(aggregateTruncMark)
	if len(dst)+len(add) <= room {
		return dst + add
	}
	if strings.HasSuffix(dst, aggregateTruncMark) {
		return dst // 已截断：后续内容直接丢弃，不再重复标记
	}
	keep := room - len(dst)
	if keep < 0 {
		keep = 0
	}
	return dst + add[:keep] + aggregateTruncMark
}

func fail(res *model.TaskResult, err error) *model.TaskResult {
	res.Failed = true
	res.Msg = err.Error()
	return res
}
