package push

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"
)

// isTransportError 判定 agent 调用错误是否传输层失败（不可达/断连/握手
// 失败/响应体中途截断）——http.Client 的网络错误为 *url.Error；HTTP 状态码
// 错误是普通 error（agent 活着，业务层拒绝），不触发自愈。
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true // 响应体传输中途断开（Decode 报 unexpected EOF，非 url.Error）
	}
	var ue *url.Error
	return errors.As(err, &ue)
}

// healIfUnreachable agent 传输层失败后的自愈：重置自举状态重走全流程，
// 让本连接的后续任务恢复可用。本次操作不重放——脚本可能已在远端执行，
// 盲目重试会重复执行非幂等任务；错误原样上抛，由调用方决定是否重跑。
// 用户主动取消/整体超时（ctx 已结束）不触发：那是运行生命周期在收尾，
// 重连没有意义且会拖慢整批主机的失败上报。
func (c *Conn) healIfUnreachable(ctx context.Context, err error) {
	if !isTransportError(err) || ctx.Err() != nil {
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if c.agent != nil {
		_ = c.agent.Close()
		c.agent = nil
	}
	c.started = false
	// 旧 agent 若只是断连仍存活，会残留至空闲超时自清理（自举注入的
	// --idle-timeout 兜底）；新自举换新随机后缀与端口，互不冲突。
	// 上限 60s：自举含整个二进制重传，慢链路下 30s 不够
	hctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if cerr := c.Connect(hctx); cerr != nil {
		fmt.Fprintf(os.Stderr, "[push] host %s agent unreachable and re-bootstrap failed: %v\n", c.host.Name, cerr)
	}
}
