package agentc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Shutdown 请求 agent 优雅退出（清理用）。
func (c *Conn) Shutdown(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/shutdown", nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("shutdown HTTP %d", resp.StatusCode)
	}
	return nil
}

// ShutdownAndCleanup 请求 agent 退出并自清理（常驻 agent 退役用）。
// unit 非空时停用指定的 systemd 单元（缺省取 agent 启动参数的单元名）；
// files 为额外一并删除的文件/目录。两者均空时发空 JSON——默认清理：
// 自身二进制、证书材料（含 CA）与默认 systemd 单元，全部 best-effort。
func (c *Conn) ShutdownAndCleanup(ctx context.Context, unit string, files []string) error {
	payload := map[string]any{}
	if unit != "" {
		payload["systemd_unit"] = unit
	}
	if len(files) > 0 {
		payload["files"] = files
	}
	body, _ := json.Marshal(payload) // 空参序列化为 {}，即默认清理
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/shutdown", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("shutdown+cleanup HTTP %d: %s", resp.StatusCode, string(msg))
	}
	return nil
}
