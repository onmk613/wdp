package conn

// 所有执行通道共用的输出封顶 writer。此前 sshc（capBuffer）与 selfrun
// （capWriter）各持一份近似拷贝、local 通道完全缺失——同一模块跨通道
// 行为分叉（local 无界 bytes.Buffer 下一条高输出命令就是控制端 OOM 的
// 最短路径），收敛为单一实现防口径漂移。

import "bytes"

// MaxExecOutputBytes 是单次执行每个输出流的缓冲上限：输出在通道层截获，
// 保留前 1MiB 并标记。模块层 truncateOut 在 Exec 返回**之后**才截断，
// 对内存保护而言太晚。
const MaxExecOutputBytes = 1 << 20

// CapWriter 是带上限的缓冲 writer：保留前 limit 字节，超出部分丢弃并
// 标记（不中断读取，"超限"作为结果上报而不是传输失败）。始终返回原始
// 写入长度——io.Writer 契约禁止 n < len(p) 且 err == nil（io.Copy 会判
// ErrShortWrite 并中断整条拷贝）。
type CapWriter struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *CapWriter) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		w.limit = MaxExecOutputBytes
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

// Truncated 报告是否发生截断。截断标记的文案含通道名（[wdp-ssh] /
// [wdp-local] / [wdp-agent]），由各通道在结果尾部自行拼接。
func (w *CapWriter) Truncated() bool { return w.truncated }

// String 返回缓冲内容（不含截断标记）。
func (w *CapWriter) String() string { return w.buf.String() }
