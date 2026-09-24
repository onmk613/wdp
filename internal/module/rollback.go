package module

// auto_rollback 的变更登记：模块在变更前快照、登记恢复/删除动作，
// executor 按逆序回放实现自动回滚。

import (
	"fmt"
	"strings"

	"wdp/internal/shellquote"
)

// RollbackAction 是一条可回滚变更：
//   - restore：Path 从 Shadow 快照恢复（覆盖前快照）
//   - remove：删除 Path（本次新建的文件/目录）
type RollbackAction struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Shadow string `json:"shadow,omitempty"`
}

// RollbackCtx 由 executor 注入：远端快照根目录 + 变更记录回调 +
// 快照失败上报（失败即该路径无法自动回滚，必须让运维看见）。
type RollbackCtx struct {
	Dir    string
	Record func(RollbackAction)
	// OnSnapshotFailure 在快照失败时调用（path + 原因）。为 nil 时只在
	// 返回值里体现，不额外上报。
	OnSnapshotFailure func(path, reason string)
}

// Snapshot 在变更前把已存在的目标快照到 shadow 区并登记 restore 动作。
// 快照失败不阻塞部署（该文件将无法自动回滚），但**必须上报**：静默吞掉
// 会让 auto_rollback 这个安全兜底"报成功却什么都没保住"——磁盘满、
// 权限不足、路径不可 cp 都会走到这里，是回滚最危险的假阴性。
// dest 必须是绝对路径：相对路径无法在 shadow 区定位（缺分隔符），
// 且模块层下发路径均为绝对路径，相对路径即调用方错误。
func (rb *RollbackCtx) Snapshot(rc *RunContext, dest string) {
	if !strings.HasPrefix(dest, "/") {
		rb.reportFailure(dest, "destination is not an absolute path")
		return
	}
	// 尾斜杠会让 pathDirOf 退化成 shadow 本身（mkdir -p 预建目标 →
	// cp -a 变"拷入"），落快照时先归一
	clean := strings.TrimRight(dest, "/")
	if clean == "" {
		clean = "/"
	}
	shadow := rb.Dir + clean
	script := fmt.Sprintf("mkdir -p -- %s && cp -a -- %s %s",
		shellquote.Quote(pathDirOf(shadow)), shellquote.Quote(clean), shellquote.Quote(shadow))
	out, bad := rc.exec(script)
	if bad != nil {
		rb.reportFailure(dest, "transfer failed: "+firstLine(bad.Msg))
		return
	}
	if out.Code != 0 {
		rb.reportFailure(dest, fmt.Sprintf("rc=%d %s", out.Code, strings.TrimSpace(out.Stderr)))
		return
	}
	rb.Record(RollbackAction{Kind: "restore", Path: clean, Shadow: shadow})
}

// reportFailure 上报一次快照失败。
func (rb *RollbackCtx) reportFailure(path, reason string) {
	if rb.OnSnapshotFailure != nil {
		rb.OnSnapshotFailure(path, reason)
	}
}

// RecordRemove 登记新建路径的删除动作。
func (rb *RollbackCtx) RecordRemove(path string) {
	rb.Record(RollbackAction{Kind: "remove", Path: path})
}

func pathDirOf(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "/"
	}
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "/"
}
