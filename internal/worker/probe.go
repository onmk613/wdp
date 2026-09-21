package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"wdp/internal/store"
)

// ProbeClient 探活专用 HTTP 客户端：短超时、禁跳转、复用连接。
// 导出给传输层复用（shutdown 推送等明文 agent 调用）。
func ProbeClient() *http.Client { return probeClient }

var probeClient = &http.Client{
	Timeout: 5 * time.Second,
	// agent 直连：环境代理（HTTP_PROXY）会把私网地址请求带向无关服务并
	// 以其响应（如 400）冒充 agent 结论
	Transport: &http.Transport{Proxy: nil},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// ProbeResult 是一次探活的结果快照（agent /health 的可见子集 + 结论）。
type ProbeResult struct {
	Status       string `json:"status"`           // online / offline
	Scheme       string `json:"scheme,omitempty"` // 接通使用的 scheme（https/http）
	Error        string `json:"error,omitempty"`
	Hostname     string `json:"hostname,omitempty"`
	Version      string `json:"version,omitempty"`  // 协议版本
	Build        string `json:"build,omitempty"`    // 二进制发布版本（远程升级比较用；空 = 旧版二进制）
	BinPath      string `json:"bin_path,omitempty"` // agent 二进制路径（升级替换目标）
	Goos         string `json:"goos,omitempty"`
	Arch         string `json:"arch,omitempty"`
	CertNotAfter string `json:"cert_not_after,omitempty"`
	CheckedAt    string `json:"checked_at"`
}

// ProbeHost 对台账主机做一次 /health 探测。纳管装好的 agent 以 mTLS
// 监听：优先用控制端客户端证书走 https；手工台账的明文 agent 回退
// http。agent 的 /health 免认证（不进入空闲计数），证书链校验由
// mTLS 客户端的 RootCAs 保证。
func ProbeHost(ctx context.Context, h *store.Host, mtls *http.Client) ProbeResult {
	if mtls != nil {
		if res, ok := probeOnce(ctx, h, "https", mtls); ok {
			return res
		}
	}
	res, _ := probeOnce(ctx, h, "http", probeClient)
	return res
}

// probeOnce 以指定 scheme 探测一次；返回 ok=false 表示该通道不可用
// （供 https→http 回退）。
func probeOnce(ctx context.Context, h *store.Host, scheme string, client *http.Client) (ProbeResult, bool) {
	res := ProbeResult{Status: "offline", Scheme: scheme, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	url := fmt.Sprintf("%s://%s/health", scheme, net.JoinHostPort(h.Address, fmt.Sprint(h.AgentPort)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		res.Error = err.Error()
		return res, false
	}
	resp, err := client.Do(req)
	if err != nil {
		res.Error = err.Error()
		return res, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		res.Error = fmt.Sprintf("health check %s: HTTP %d %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
		return res, false
	}
	var info struct {
		Ok           bool   `json:"ok"`
		Hostname     string `json:"hostname"`
		Version      string `json:"version"`
		Build        string `json:"build"`
		BinPath      string `json:"bin_path"`
		Goos         string `json:"goos"`
		Arch         string `json:"arch"`
		CertNotAfter string `json:"cert_not_after"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<10)).Decode(&info); err != nil {
		res.Error = "decode health: " + err.Error()
		return res, false
	}
	res.Status = "online"
	res.Hostname, res.Version, res.Build, res.BinPath, res.Goos, res.Arch, res.CertNotAfter =
		info.Hostname, info.Version, info.Build, info.BinPath, info.Goos, info.Arch, info.CertNotAfter
	return res, true
}

// Prober 周期探活全量主机并写回状态。Probe 回调由传输层注入（带 mTLS
// 客户端的探测），返回 (状态, 错误串)。
type Prober struct {
	Store  *store.Store
	Logger *slog.Logger
	Every  time.Duration
	Probe  func(ctx context.Context, h *store.Host) ProbeResult
}

// Run 阻塞执行探活循环（ctx 取消退出；启动即先探一轮）。
// 有界并发（与 Monitor 采样同款）：串行时单台最坏 ~10s（mTLS+明文两趟
// 超时），离线主机一多一轮远超周期。
func (p *Prober) Run(ctx context.Context) {
	probe := func() {
		ids, err := p.Store.HostIDs()
		if err != nil {
			p.Logger.Error("probe: list hosts", "err", err)
			return
		}
		sem := make(chan struct{}, 8)
		var wg sync.WaitGroup
		for _, id := range ids {
			if ctx.Err() != nil {
				break
			}
			h, err := p.Store.GetHost(id)
			if err != nil {
				continue
			}
			wg.Add(1)
			go func(id int64, h *store.Host) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				res := p.Probe(ctx, h)
				if err := p.Store.SetHostStatus(id, res.Status); err != nil {
					p.Logger.Error("probe: set status", "host", h.Name, "err", err)
				}
				p.Logger.Debug("probe done", "host", h.Name, "status", res.Status, "err", res.Error)
			}(id, h)
		}
		wg.Wait()
	}
	probe() // 启动即先探一轮
	t := time.NewTicker(p.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			probe()
		}
	}
}
