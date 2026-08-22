package executor

import (
	"fmt"
	"wdp/internal/i18n"
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
	n := *r
	return &n
}

// maxOutLen 是单任务捕获输出的上限（防 cat 大文件撑爆内存）。
const maxOutLen = 1 << 20

// truncateOut 截断超长输出并标注。
func truncateOut(s string) string {
	if len(s) <= maxOutLen {
		return s
	}
	return s[:maxOutLen] + fmt.Sprintf(i18n.T("\n…[wdp] output truncated (%d bytes)", "\n…[wdp] 输出超长已截断（%d 字节）"), len(s))
}

func fail(res *model.TaskResult, err error) *model.TaskResult {
	res.Failed = true
	res.Msg = err.Error()
	return res
}
