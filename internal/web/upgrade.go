package web

// agent 远程升级：server 经 agent 通道（PUT /file + POST /exec，旧版 agent
// 已具备）把新二进制推到目标机做原子替换并重启服务。流程完全在 server
// 侧编排——这正是让旧 agent 享受新能力（如 /metrics）的通道：升级一次，
// 之后的新接口才可用。
//
//	探活（拿平台/版本）→ bin 目录找同级二进制 → 版本相同且未 force 短路
//	→ 上传临时文件 → mv 原子替换正在运行的二进制（Linux rename 语义安全，
//	  进程仍持旧 inode）→ systemd restart（会杀掉 agent，exec 连接中断是
//	  预期，忽略）→ 轮询探活直到新版本上线。
//
// 非 systemd 部署（手动前台运行，如演练环境）：只替换二进制并提示手动
// 重启——没人负责拉起新进程，这是部署形态限制。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wdp/internal/agent"
	"wdp/internal/conn"
	"wdp/internal/conn/agentc"
	"wdp/internal/shellquote"
	"wdp/internal/store"
)

// UpgradeResult 单台升级结果。
type UpgradeResult struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	From   string `json:"from"` // 升级前 build（空 = 旧版二进制未上报）
	To     string `json:"to"`
	Detail string `json:"detail,omitempty"`
}

// handleUpgradeHost 单台升级：POST /api/hosts/{id}/upgrade {"force":bool}。
func (s *Server) handleUpgradeHost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	h, err := s.st.GetHost(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "host not found")
		return
	}
	// 执行闸门：升级替换二进制期间不能并行跑部署脚本（同步请求限时获取）
	release, ok := s.gate.TryAcquire([]int64{h.ID}, 3*time.Second)
	if !ok {
		writeError(w, http.StatusConflict, "主机正被其它执行占用，稍后重试")
		return
	}
	defer release()
	var req struct {
		Force bool `json:"force"`
	}
	if r.Body != nil {
		_ = decodeJSONBody(w, r, &req)
	}
	res := s.upgradeAgent(r.Context(), h, req.Force)
	if res.OK {
		s.audit(r, "upgrade", "agent", h.Name, fmt.Sprintf("%s → %s", orUnknown(res.From), res.To))
	} else {
		s.audit(r, "upgrade_failed", "agent", h.Name, res.Detail)
	}
	code := http.StatusOK
	if !res.OK {
		code = http.StatusBadGateway
	}
	writeJSON(w, code, res)
}

// decodeJSONBody 与 decodeJSON 同口径：上限 1MiB（无上限的解码可被恶意
// 大体撑爆内存）。
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v)
}

func orUnknown(v string) string {
	if v == "" {
		return "(旧版)"
	}
	return v
}

// upgradeAgent 执行单台升级全流程（阻塞；含最长 90s 的新版上线等待）。
func (s *Server) upgradeAgent(ctx context.Context, h *store.Host, force bool) UpgradeResult {
	res := UpgradeResult{ID: h.ID, Name: h.Name, To: agent.BuildVersion()}

	// 1. 探活：平台与当前版本
	pr := probeHost(ctx, h, s.mtlsProbeClient())
	if pr.Status != "online" {
		res.Detail = "agent 不可达：" + pr.Error
		return res
	}
	res.From = pr.Build
	platform := platformKey(pr.Goos, pr.Arch)
	if platform == "" {
		res.Detail = fmt.Sprintf("不支持的平台 %s/%s", pr.Goos, pr.Arch)
		return res
	}

	// 2. 版本相同且未强制 → 幂等成功（无需二进制参与，先于 resolver 判断）
	if !force && pr.Build == res.To {
		res.OK = true
		res.Detail = "已是最新版本 " + res.To
		return res
	}

	// 3. server 同级 bin 目录找目标平台二进制（与 enroll 推装同一来源）
	binPath, ok := s.binResolver(platform)
	if !ok {
		res.Detail = fmt.Sprintf("server 缺少 %s 二进制（须从 build.sh 产出的 bin 目录运行）", platform)
		return res
	}

	// 4. 上传到替换目标同目录的临时文件：同文件系统保证 mv 是 rename
	//    （跨设备会退化成 cp+rm，cp 写运行中的 ELF 得 ETXTBY）。目标
	//    路径优先取 agent 上报的 bin_path；旧版 agent 未上报时由脚本
	//    readlink /proc/$PPID/exe 探测（darwin 无 /proc，兜底 /usr/local/bin）。
	dst := pr.BinPath
	dir := "/usr/local/bin"
	if dst != "" {
		dir = filepath.Dir(dst)
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	tmp := filepath.Join(dir, fmt.Sprintf(".wdp-upgrade-%s", hex.EncodeToString(suffix)))
	scheme := s.agentScheme(ctx, h)
	dc := &conn.Defaults{Conn: "agent"}
	ac := agentc.New(s.agentHostModel(h, scheme), dc)
	defer ac.Close()
	f, err := os.Open(binPath)
	if err != nil {
		res.Detail = "打开二进制失败: " + err.Error()
		return res
	}
	uctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	if err := ac.UploadFile(uctx, tmp, f, 0o755); err != nil {
		cancel()
		f.Close()
		res.Detail = "上传失败: " + err.Error()
		return res
	}
	f.Close()
	cancel()

	// 5. 原子替换 + 重启。systemd restart 会杀掉 agent 自身 → 该 exec 的
	//    HTTP 响应大概率中断，错误是预期，转入探活等待。
	script := upgradeScript(tmp, dst)
	out, execErr := ac.Exec(ctx, conn.ExecRequest{Script: script, TimeoutMs: 30000})
	manualRestart := execErr == nil && strings.Contains(out.Stdout, "WDP_NO_SYSTEMD")

	// 6. 等待新版上线（systemd restart 后服务需数秒拉起）
	if !manualRestart {
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if ctx.Err() != nil {
				res.Detail = "等待被取消"
				return res
			}
			time.Sleep(3 * time.Second)
			p2 := probeHost(ctx, h, s.mtlsProbeClient())
			if p2.Status == "online" {
				if p2.Build == res.To {
					res.OK = true
					res.Detail = fmt.Sprintf("%s → %s，已重启上线", orUnknown(res.From), res.To)
					return res
				}
				// 上线了但还是旧 build：二进制没换成功（路径不对/权限）
				res.Detail = fmt.Sprintf("agent 已重启但版本仍为 %s（二进制替换未生效，检查安装路径）", orUnknown(p2.Build))
				return res
			}
		}
		res.Detail = "替换已下发但 agent 未在 90s 内恢复（systemd 拉起失败？在目标机查 systemctl status wdp-agent）"
		return res
	}
	// 非 systemd：二进制已就位，旧进程还在跑旧代码
	res.OK = true
	res.Detail = fmt.Sprintf("二进制已更新到 %s；该 agent 非 systemd 托管，需手动重启进程生效", res.To)
	return res
}

// platformKey 由 /health 的 goos/arch 拼平台键（linux_amd64）。
func platformKey(goos, arch string) string {
	key := strings.ToLower(goos + "_" + arch)
	if !platformKeyRe.MatchString(key) {
		return ""
	}
	return key
}

// upgradeScript 生成替换脚本：dst 为空（旧版 agent 未上报路径）时经
// /proc/$PPID/exe 探测（exec 的 sh 是 agent 的子进程）；mv 原子替换
// （不能直接写：运行中的 ELF 写打开会 ETXTBSY），systemd 托管则 restart。
// dst 来自 agent 上报（不可信输入）且 tmp 与之间目录：一律单引号字面量
// 引用——Go %q 的转义与 sh 双引号语义不同（$、反引号、\x）。
func upgradeScript(tmp, dst string) string {
	setDst := `DST=$(readlink /proc/$PPID/exe 2>/dev/null || echo /usr/local/bin/wdp)`
	if dst != "" {
		setDst = "DST=" + shellquote.Quote(dst)
	}
	return fmt.Sprintf(`set -e
%[2]s
mv -f %[3]s "$DST" && chmod 755 "$DST"
if systemctl list-unit-files 2>/dev/null | grep -q '^%[4]s'; then
  systemctl restart %[4]s && echo "WDP_RESTARTED"
else
  echo "WDP_NO_SYSTEMD"
fi`, tmp, setDst, shellquote.Quote(tmp), agentUnitName)
}

// handleUpgradeBatch 批量升级（POST /api/hosts/upgrade {"ids":[],"force":bool}）：
// 并发 3（每台含二进制上传与重启等待）。
func (s *Server) handleUpgradeBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs   []int64 `json:"ids"`
		Force bool    `json:"force"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is empty")
		return
	}
	hosts := make([]*store.Host, 0, len(req.IDs))
	for _, id := range req.IDs {
		if h, err := s.st.GetHost(id); err == nil {
			hosts = append(hosts, h)
		}
	}
	results := make([]UpgradeResult, len(hosts))
	var (
		mu     sync.Mutex
		cursor int
		wg     sync.WaitGroup
		sem    = make(chan struct{}, 3)
	)
	for range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			mu.Lock()
			i := cursor
			h := hosts[i]
			cursor++
			mu.Unlock()
			results[i] = s.upgradeAgent(r.Context(), h, req.Force)
		}()
	}
	wg.Wait()
	okN := 0
	for _, res := range results {
		if res.OK {
			okN++
		}
	}
	s.audit(r, "batch_upgrade", "agent", fmt.Sprintf("%d 台", len(hosts)), fmt.Sprintf("成功 %d / %d", okN, len(results)))
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "ok": okN, "failed": len(results) - okN})
}
