package sshc

// 会话输出上限：实现收敛至 conn.CapWriter（所有执行通道同一口径——
// 远端输出在会话层截获，保留前 1MiB 并标记。此前 bytes.Buffer 全量驻留
// 内存，一条高输出命令（yes、cat 大文件）就是控制端 OOM 的最短路径；
// 模块层 truncateOut 在 Exec 返回**之后**才截断，对内存保护而言太晚）。
// 本文件只保留 sshc 侧的截断标记文案。

import "wdp/internal/conn"

// capped 返回会话输出；发生截断时在尾部追加可见标记（调用方能从结果
// 看出输出不完整，而不是拿到一段"正常"的半截输出）。
func capped(w *conn.CapWriter) string {
	s := w.String()
	if w.Truncated() {
		s += "\n[wdp-ssh] output exceeded 1MiB and was truncated"
	}
	return s
}
