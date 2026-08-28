package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// shutdownReq 是 POST /shutdown 的可选请求体：
//   - 空 body / 空 JSON（{} 或 null）：默认自清理——删除自身二进制与证书
//     材料（cert/key/CA）、尝试停用 systemd 单元（缺省 --systemd-unit 名）。
//     全部 best-effort：单项失败只记日志不阻断其余步骤，非 systemd 环境
//     静默跳过
//   - systemd_unit：覆盖要停用的 systemd 单元名
//   - files：额外一并删除的文件或目录
type shutdownReq struct {
	SystemdUnit string   `json:"systemd_unit"`
	Files       []string `json:"files"`
}

// handleShutdown 优雅退出并自清理。认证模型与其余端点一致：无认证模式
// 直接执行；mTLS 模式经客户端证书（及 pin 名单）校验后执行，无额外开关。
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	var req shutdownReq
	// body 可省（push Close 与旧控制端不携带）；有内容时必须是合法 JSON
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		http.Error(w, "failed to read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "request body parse failed: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	s.logInfo("shutdown requested, self-cleanup follows (systemd unit %q, %d extra path(s))", s.cleanupUnit(req.SystemdUnit), len(req.Files))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	go func() {
		time.Sleep(200 * time.Millisecond) // 等响应送达
		s.initiateShutdown(req, true)
	}()
}

// initiateShutdown 执行清理并关停服务（/shutdown 与空闲看门狗共用路径）。
// cleanup 为 false 时仅关停不清理（空闲退出未配 --cleanup-on-shutdown 的
// 常驻 agent 用）；/shutdown 显式信号恒为 true。
func (s *Server) initiateShutdown(req shutdownReq, cleanup bool) {
	if cleanup {
		s.performCleanup(req)
	}
	if srv := s.httpSrv.Load(); srv != nil {
		// 限时等待活动连接排空：无限期 Shutdown 会被长任务永远挂住，
		// 超时后强制关停全部连接（自清理后进程退出，残留任务随之终结）
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		_ = srv.Close()
	}
	// httpSrv 为 nil（Handler 被外部嵌入测试）时仅作罢，不退出进程
}

// performCleanup 自清理，全部 best-effort。先停 systemd 单元再删文件：
// Restart=always 的单元在文件删除后会陷入启动失败循环。
func (s *Server) performCleanup(req shutdownReq) {
	s.logInfo("self-cleanup starting (systemd unit %q, %d extra path(s)); log file (if any) is kept for audit", s.cleanupUnit(req.SystemdUnit), len(req.Files))
	s.stopSystemService(s.cleanupUnit(req.SystemdUnit))
	s.removeCertFiles()
	s.removeBootstrapSidecars()
	s.removeExtraPaths(req.Files)
	s.removeSelfBinary()
}

// removeBootstrapSidecars 删除 push 自举的附属文件（启动脚本生成的
// <bin>.log 与 <bin>.pid；自删清单原本不含它们，成功自举后会在目标机
// /tmp 残留）。常驻 agent 无这些文件，静默跳过。
func (s *Server) removeBootstrapSidecars() {
	if s.selfBin == "" {
		return
	}
	for _, suffix := range []string{".log", ".pid"} {
		if err := os.Remove(s.selfBin + suffix); err != nil && !os.IsNotExist(err) {
			s.logWarn("cleanup failed to remove %s%s: %v", s.selfBin, suffix, err)
		}
	}
}

// cleanupUnit 归一化要停用的单元名：请求指定优先，缺省取启动参数。
func (s *Server) cleanupUnit(reqUnit string) string {
	if reqUnit != "" {
		return reqUnit
	}
	if s.systemdUnit != "" {
		return s.systemdUnit
	}
	return "wdp-agent"
}

// removeCertFiles 删除自身证书材料（cert/key/CA，自清理语义下 CA 一并下线）。
func (s *Server) removeCertFiles() {
	for _, f := range []string{s.tlsCertFile, s.tlsKeyFile, s.tlsCAFile} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			s.logWarn("cleanup failed to remove %s: %v", f, err)
		}
	}
}

// removeExtraPaths 删除调用方指定的文件或目录（远程清理附加项）。
// 空串跳过；解析到根路径拒删（手滑防呆）。RemoveAll 兼容文件与目录。
func (s *Server) removeExtraPaths(paths []string) {
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil && abs == string(filepath.Separator) {
			s.logWarn("cleanup refused to remove root path %s", p)
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			s.logWarn("cleanup failed to remove %s: %v", p, err)
		}
	}
}

// removeSelfBinary 删除自身二进制。运行中的进程在 Unix 上可自删；
// Windows 不允许删除运行中映像，退化为改名踢出原路径（下次重启后可删，
// 服务不再能按原路径启动）。
func (s *Server) removeSelfBinary() {
	if s.selfBin == "" {
		return
	}
	if err := os.Remove(s.selfBin); err == nil {
		return
	}
	if err := os.Rename(s.selfBin, s.selfBin+".wdp-agent-deleted"); err != nil {
		s.logWarn("cleanup failed to remove own binary %s: %v", s.selfBin, err)
	}
}

// stopSystemService best-effort 停用 systemd 托管的单元（避免
// Restart=always 在文件删除后进入重启失败循环）。非 systemd 环境
// （无 systemctl 或 /run/systemd/system 不存在）静默跳过。
func (s *Server) stopSystemService(unit string) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return // 非 systemd 环境（容器/非 Linux）
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "systemctl", "disable", "--now", unit).Run(); err != nil {
		s.logWarn("cleanup failed to disable systemd unit %s: %v", unit, err)
	}
}
