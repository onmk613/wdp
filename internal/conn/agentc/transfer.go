package agentc

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"wdp/internal/conn"
)

// UploadFile 调用 agent 的 /file 上传。
func (c *Conn) UploadFile(ctx context.Context, dst string, r io.Reader, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	url := fmt.Sprintf("%s/file?path=%s&mode=%o", c.base, queryEscape(dst), mode.Perm())
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, r)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("agent upload HTTP %d: %s", resp.StatusCode, string(msg))
	}
	return nil
}

// DownloadFile 调用 agent 的 /file 下载。
func (c *Conn) DownloadFile(ctx context.Context, src string, w io.Writer) error {
	url := fmt.Sprintf("%s/file?path=%s", c.base, queryEscape(src))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("agent download HTTP %d: %s", resp.StatusCode, string(msg))
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// NativeExtract 调用 agent 的 /archive 原生解压（远端 Go 实现，不依赖
// 目标机工具链）。旧版常驻 agent 无该端点（404/405）时返回
// conn.ErrNativeUnsupported，调用方回退 shell 路径；其余错误真实上抛。
func (c *Conn) NativeExtract(ctx context.Context, src, dest string) error {
	url := fmt.Sprintf("%s/archive?src=%s&dest=%s", c.base, queryEscape(src), queryEscape(dest))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return conn.ErrNativeUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("agent extract HTTP %d: %s", resp.StatusCode, string(msg))
	}
	return nil
}

func queryEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') ||
			ch == '-' || ch == '_' || ch == '.' || ch == '~' || ch == '/' {
			b.WriteByte(ch)
		} else {
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}
