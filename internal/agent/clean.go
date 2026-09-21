package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
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

// performCleanup 自清理，全部 best-effort。顺序是生存要点：
//
//	忽略停止信号 → disable 单元（不 stop）→ 删文件（证书/附属/自身
//	二进制/单元文件 + daemon-reload）→ --no-block stop
//
// 本进程即该 systemd 单元的主进程——若先 systemctl stop，SIGTERM 会在
// 删除任何文件之前杀死正在执行清理的进程（现象：服务停了、文件全在）。
// 因此先忽略 SIGTERM/SIGINT 保全自身，disable（不触发信号）断开开机
// 自启，完成全部删除（含单元文件，daemon-reload 让 systemd 忘掉该
// 定义）后用 --no-block stop 异步停用：信号被忽略，systemd 在本进程
// 自然退出后收尾。Restart=always 不会复活：单元是被显式 stop 的，不属
// 意外退出。
func (s *Server) performCleanup(req shutdownReq) {
	s.logInfo("self-cleanup starting (systemd unit %q, %d extra path(s)); log file (if any) is kept for audit", s.cleanupUnit(req.SystemdUnit), len(req.Files))
	signal.Ignore(syscall.SIGTERM, syscall.SIGINT) // 清理期间不得被停止信号打断
	unit := s.cleanupUnit(req.SystemdUnit)
	s.disableSystemService(unit)
	s.removeCertFiles()
	s.removeBootstrapSidecars()
	s.removeExtraPaths(req.Files)
	s.removeSelfBinary()
	s.removeUnitFile(unit)
	s.stopSystemServiceNoBlock(unit)
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

// removeCertFiles 删除自身证书材料（cert/key/CA，自清理语义下 CA 一并下线），
// 随后best-effort移除已空的证书目录（非空则保留，不影响其他部署物）。
func (s *Server) removeCertFiles() {
	for _, f := range []string{s.tlsCertFile, s.tlsKeyFile, s.tlsCAFile} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			s.logWarn("cleanup failed to remove %s: %v", f, err)
		}
	}
	if dir := filepath.Dir(s.tlsCertFile); dir != "." && dir != "/" {
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			s.logDebug("cert dir %s not removed (non-empty or other): %v", dir, err)
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

// systemdAvailable 非 systemd 环境（无 systemctl 或 /run/systemd/system
// 不存在）返回 false，相关步骤静默跳过。
func systemdAvailable() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	_, err := os.Stat("/run/systemd/system")
	return err == nil
}

// disableSystemService 断开单元的开机自启（disable 不含 stop：不向本
// 进程发信号）。
func (s *Server) disableSystemService(unit string) {
	if !systemdAvailable() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "systemctl", "disable", unit).Run(); err != nil {
		s.logWarn("cleanup failed to disable systemd unit %s: %v", unit, err)
	}
}

// unitFileName 归一单元文件名：无后缀的单元名补 .service。
func unitFileName(unit string) string {
	if !strings.HasSuffix(unit, ".service") {
		return unit + ".service"
	}
	return unit
}

// unitFilePath 定位单元文件：优先问 systemd（FragmentPath，覆盖
// /usr/lib/systemd 等非默认安装位置），不可得时回退
// /etc/systemd/system/<unit>.service（本工具安装脚本的固定落点）。
func (s *Server) unitFilePath(unit string) string {
	if systemdAvailable() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "systemctl", "show", unit, "-P", "FragmentPath", "--no-pager").Output(); err == nil {
			if p := strings.TrimSpace(string(out)); p != "" {
				return p
			}
		}
	}
	return filepath.Join("/etc/systemd/system", unitFileName(unit))
}

// removeUnitFile 删除单元文件并 daemon-reload（systemd 忘掉该单元定义，
// 退役不留残文件；运行中的实例不受影响，随后 --no-block stop 收尾）。
func (s *Server) removeUnitFile(unit string) {
	if !systemdAvailable() {
		return
	}
	p := s.unitFilePath(unit)
	if err := os.Remove(p); err != nil {
		if !os.IsNotExist(err) {
			s.logWarn("cleanup failed to remove unit file %s: %v", p, err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "systemctl", "daemon-reload").Run(); err != nil {
		s.logWarn("cleanup failed to daemon-reload after unit removal: %v", err)
	}
}

// stopSystemServiceNoBlock 文件删除完毕后异步停用单元（--no-block 立即
// 返回不等停完成；SIGTERM 已被忽略，systemd 在本进程退出后收尾）。
func (s *Server) stopSystemServiceNoBlock(unit string) {
	if !systemdAvailable() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "systemctl", "--no-block", "stop", unit).Run(); err != nil {
		s.logWarn("cleanup failed to stop systemd unit %s: %v", unit, err)
	}
}
