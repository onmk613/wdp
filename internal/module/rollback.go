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

// RollbackCtx 由 executor 注入：远端快照根目录 + 变更记录回调。
type RollbackCtx struct {
	Dir    string
	Record func(RollbackAction)
}

// Snapshot 在变更前把已存在的目标快照到 shadow 区并登记 restore 动作。
// 快照失败不阻塞部署（该文件将无法自动回滚）。
// dest 必须是绝对路径：相对路径无法在 shadow 区定位（缺分隔符），
// 且模块层下发路径均为绝对路径，相对路径即调用方错误。
func (rb *RollbackCtx) Snapshot(rc *RunContext, dest string) {
	if !strings.HasPrefix(dest, "/") {
		return
	}
	shadow := rb.Dir + dest
	script := fmt.Sprintf("mkdir -p -- %s && cp -a -- %s %s",
		shellquote.Quote(pathDirOf(shadow)), shellquote.Quote(dest), shellquote.Quote(shadow))
	out, bad := rc.exec(script)
	if bad != nil || out.Code != 0 {
		return
	}
	rb.Record(RollbackAction{Kind: "restore", Path: dest, Shadow: shadow})
}

// RecordRemove 登记新建路径的删除动作。
func (rb *RollbackCtx) RecordRemove(path string) {
	rb.Record(RollbackAction{Kind: "remove", Path: path})
}

func pathDirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "/"
}
