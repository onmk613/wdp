package agentc

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// Logs 拉取 agent 近期日志（GET /logs：内存环形缓冲，约最近 512KiB；
// 完整历史需目标 agent 以 --log-file 落盘后经 GET /file 拉取）。
func (c *Conn) Logs(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/logs", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("agent logs HTTP %d: %s", resp.StatusCode, string(msg))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}
