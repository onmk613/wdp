package sshc

// 会话输出上限：与 agent 通道（selfrun maxExecOutputBytes）同一口径——
// 远端输出在会话层截获，保留前 1MiB 并标记。此前 bytes.Buffer 全量驻留
// 内存，一条高输出命令（yes、cat 大文件）就是控制端 OOM 的最短路径；
// 模块层 truncateOut 在 Exec 返回**之后**才截断，对内存保护而言太晚。

import "bytes"

// maxExecOutputBytes 是单次会话每个输出流的缓冲上限。
const maxExecOutputBytes = 1 << 20

// capBuffer 是带上限的缓冲 writer：保留前 limit 字节，超出部分丢弃并标记
// （不中断读取，"超限"作为结果上报而不是传输失败）。始终返回原始写入
// 长度——io.Writer 契约禁止 n < len(p) 且 err == nil（io.Copy 会判
// ErrShortWrite 并中断整条拷贝）。
type capBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *capBuffer) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		w.limit = maxExecOutputBytes
	}
	room := w.limit - w.buf.Len()
	switch {
	case room <= 0:
		w.truncated = true
	case len(p) > room:
		w.buf.Write(p[:room])
		w.truncated = true
	default:
		w.buf.Write(p)
	}
	return len(p), nil
}

// string 返回缓冲内容；发生截断时在尾部追加可见标记（调用方能从结果
// 看出输出不完整，而不是拿到一段"正常"的半截输出）。
func (w *capBuffer) string() string {
	s := w.buf.String()
	if w.truncated {
		s += "\n[wdp-ssh] output exceeded 1MiB and was truncated"
	}
	return s
}
