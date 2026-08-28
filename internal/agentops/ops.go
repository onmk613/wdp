package agentops

// agent 日常运维内核：证书巡检（status）、远程退役自清理（retire）、
// 日志拉取（logs）。操作对象是 conn: agent 的主机（主机来源选取在
// internal/cli 的 agentctl 命令层）。

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wdp/internal/conn"
	"wdp/internal/conn/agentc"
	"wdp/internal/model"
)

// forEachHost 按并发上限对每台主机执行 op（单台失败不中断批次；返回失败数）。
func forEachHost(ctx context.Context, hosts []*model.Host, forks int, op func(ctx context.Context, h *model.Host) error) int {
	if forks <= 0 {
		forks = 5
	}
	sem := make(chan struct{}, forks)
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed int
	)
	for _, h := range hosts {
		wg.Add(1)
		go func(h *model.Host) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := op(ctx, h); err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				fmt.Fprintf(os.Stderr, "[agentctl] %s: %v\n", h.Name, err)
			}
		}(h)
	}
	wg.Wait()
	return failed
}

// parseCertNotAfter 解析 /health 的到期时刻。
func parseCertNotAfter(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Status 批量巡检证书剩余有效期与空闲自退出剩余时间。
func Status(ctx context.Context, hosts []*model.Host, forks int, dc *conn.Defaults, out io.Writer) error {
	fmt.Fprintln(out, "host\tcert expiry\tdays left\tidle left\tversion")
	failed := forEachHost(ctx, hosts, forks, func(ctx context.Context, h *model.Host) error {
		conn := agentc.New(h, dc)
		defer conn.Close()
		info, err := conn.Health(ctx)
		if err != nil {
			return err
		}
		expiry, days := "no mTLS", "-"
		if t, ok := parseCertNotAfter(info.CertNotAfter); ok {
			days = fmt.Sprintf("%d", int(time.Until(t).Hours()/24)+1)
			if time.Until(t) < 7*24*time.Hour {
				days += "  << RENEW"
			}
			expiry = t.Local().Format("2006-01-02")
		}
		idle := "-"
		if info.IdleTimeoutSec > 0 {
			idle = fmt.Sprintf("%dm", info.IdleLeftSec/60)
		}
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", h.Name, expiry, days, idle, info.Version)
		return nil
	})
	return summarize(failed, len(hosts))
}

// Retire 逐主机远程退役：shutdown + 自清理（默认清 agent 二进制、证书
// 材料含 CA 并停用 systemd 单元；unit 空取 agent 启动时的单元名，files
// 为额外要删的路径）。确认交互在命令层。
func Retire(ctx context.Context, hosts []*model.Host, unit string, files []string, forks int, dc *conn.Defaults) error {
	failed := forEachHost(ctx, hosts, forks, func(ctx context.Context, h *model.Host) error {
		conn := agentc.New(h, dc)
		defer conn.Close()
		return conn.ShutdownAndCleanup(ctx, unit, files)
	})
	return summarize(failed, len(hosts))
}

// Logs 逐主机拉取近期日志（GET /logs 内存缓冲）写本地文件。
// 仅内存近期缓冲；完整历史需目标机 agent 配 --log-file，再经本命令
// 或 GET /file 拉取该文件。
func Logs(ctx context.Context, hosts []*model.Host, outDir string, forks int, dc *conn.Defaults, out io.Writer) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	failed := forEachHost(ctx, hosts, forks, func(ctx context.Context, h *model.Host) error {
		conn := agentc.New(h, dc)
		defer conn.Close()
		b, err := conn.Logs(ctx)
		if err != nil {
			return err
		}
		p := filepath.Join(outDir, logFileName(h.Name))
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: %s (%d bytes)\n", h.Name, p, len(b))
		return nil
	})
	return summarize(failed, len(hosts))
}

// logFileName 主机名归一化为安全文件名（路径分隔符等替换为 _）。
func logFileName(host string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '.', r == '_':
			return r
		}
		return '_'
	}, host) + ".log"
}

// certNameFor 返回主机对应的证书名（签发时的 --name 约定：逻辑身份 =
// host 字段；inventory 未填 host 时 Address 已缺省为主机名）。
func certNameFor(h *model.Host) string {
	if h.Address != "" && h.Address != h.Name {
		return h.Address
	}
	return h.Name
}

// summarize 失败主机数 > 0 时返回错误（退出码非零）。
func summarize(failed, total int) error {
	if failed > 0 {
		return fmt.Errorf("%d/%d host(s) failed", failed, total)
	}
	return nil
}
