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
//
// 升级是后台 run（kind=upgrade）而非同步长请求：单台最长约 7 分钟（探活
// 5s + 上传 5min + exec 30s + 上线等待 90s），百台批量并发 3 要 30 分钟
// 以上——同步等待的挂起请求必然被反代/浏览器掐断，断连后前端也无处恢复
// 进度。现在受理即建 run 返回 run_id（模式与 exec 一致，见 exec.go
// handleExec），执行挂 background ctx 而非请求 ctx（动机见 httpx.go
// background()：断连取消会让远端停在半完成态，比断连更糟）。每台主机
// 一条 run：selector 记触达主机（run:view 裁剪与取消越权守卫同口径）、
// AppName 记主机名、Version 记目标版本，逐步进度落 run_tasks
// （probe/binary/upload/replace/wait-online），终态 succeeded/failed/
// cancelled + SSE 事件（runevents.go）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wdp/internal/agent"
	"wdp/internal/buildinfo"
	"wdp/internal/conn"
	"wdp/internal/conn/agentc"
	"wdp/internal/shellquote"
	"wdp/internal/store"
)

// upgradeRunKind runs 表升级行的 kind。复用既有 run 治理而非新表：列表/
// 详情/保留策略/启动对账（ReconcileStaleRuns）/SSE 事件全部自动覆盖，
// run:view 作用域按 selector ids 裁剪、取消端点按 run:execute 交集把关。
const upgradeRunKind = "upgrade"

// UpgradeResult 单台升级结果（run 推进过程的数据载体与终态摘要来源；
// 不再直接作为 HTTP 响应——受理响应只有 run_id）。
type UpgradeResult struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	From   string `json:"from"` // 升级前 build（空 = 旧版二进制未上报）
	To     string `json:"to"`
	Detail string `json:"detail,omitempty"`
}

// handleUpgradeHost 单台升级：POST /api/hosts/{id}/upgrade {"force":bool}。
// 受理即建 run 返回 run_id，升级步骤在后台 goroutine 推进；执行闸门在
// 受理期获取（忙则 409 快速反馈，用户稍后重试），随后台执行结束释放。
func (s *Server) handleUpgradeHost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	h, err := s.st.GetHost(id)
	if err != nil {
		// DB 故障≠主机不存在（writeStoreErr 分流，不再一律 404 把 500
		// 掩盖成"删过了"误导运维）
		s.writeStoreErr(w, err)
		return
	}
	var req struct {
		Force bool `json:"force"`
	}
	if r.Body != nil {
		// 空/坏 JSON 不再静默按 force=false 继续：decode 失败即 400/413
		//（与包内其他端点同口径，decodeJSON 失败时已写好响应）
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	// 执行闸门：升级替换二进制期间不能并行跑部署脚本。单台保留受理期
	// 限时获取（3s）——同步 409 的反馈最快；批量路径无法逐台限时获取
	//（百台请求本身会被闸门拖挂），改为后台排队（handleUpgradeBatch）
	release, ok := s.gate.TryAcquire([]int64{h.ID}, 3*time.Second)
	if !ok {
		writeError(w, http.StatusConflict, "主机正被其它执行占用，稍后重试")
		return
	}
	user, _ := r.Context().Value(ctxUser{}).(string)
	runID, err := s.createUpgradeRun(h, "running", user)
	if err != nil {
		release()
		s.writeInternal(w, err)
		return
	}
	ip := s.remoteIP(r)
	go func() {
		defer release()
		s.registerRunCancels([]int64{runID})
		defer s.unregisterRunCancel(runID)
		s.runUpgradeRun(runID, h, req.Force, user, ip)
	}()
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID})
}

// createUpgradeRun 建一条升级 run。selector 记实际触达的单台主机（与
// exec/app run 同口径——run:view 可见性、取消越权守卫都按它判定），
// AppName 记主机名、Version 记目标版本：runs 列表与主机任务视图直接
// 可读；升级前版本进终态摘要与 run_tasks 明细（探活时才可知，受理期
// 不落库）。
func (s *Server) createUpgradeRun(h *store.Host, status, user string) (int64, error) {
	sel, _ := json.Marshal(map[string]any{"kind": "hosts", "ids": []int64{h.ID}})
	return s.st.CreateRun(store.RunInput{
		Kind: upgradeRunKind, AppName: h.Name, Version: agent.BuildVersion(),
		Status: status, Selector: string(sel), User: user,
	})
}

// runUpgradeRun 单个升级 run 的后台推进（闸门由调用方持有）：武装取消 →
// 逐步执行并落 run_tasks → 终态收尾。取消语义同 app run：在步骤边界生效
// （探活间隙/上传/exec/等待循环响应 ctx 取消），已下发的 mv+restart 不回滚
// ——半完成态没有安全回退，取消后重新发起即幂等对账。
func (s *Server) runUpgradeRun(runID int64, h *store.Host, force bool, user, ip string) {
	ctx, cancel := context.WithCancel(s.background())
	defer cancel()
	if !s.armRunCancel(runID, cancel) {
		s.finishUpgradeRun(runID, h, "cancelled", "用户取消（排队阶段，未执行任何步骤）", user, ip)
		return
	}
	_ = s.st.SetRunStatus(runID, "running")
	s.runs.notify(runEvent{ID: runID, Status: "running"})
	res := s.upgradeAgent(ctx, h, force, func(task, status, detail string) {
		// run_tasks 是进度与审计面：落库失败（磁盘满/库锁）不能中断升级
		// 本身，但也不能无声——与 exec 路径（console/exec.go）同口径留告警
		if terr := s.st.AddRunTask(&store.RunTask{
			RunID: runID, Play: "upgrade", Task: task, Module: "agent",
			Host: h.Name, Status: status, Detail: truncate(detail, 16<<10),
		}); terr != nil {
			s.logger.Warn("run_tasks audit write failed",
				"run_id", runID, "host", h.Name, "step", task, "err", terr)
		}
		s.runs.notify(runEvent{ID: runID, Status: "running"})
	})
	status, summary := "succeeded", res.Detail
	if !res.OK {
		status = "failed"
	}
	if s.runCancelRequested(runID) {
		// ctx 在步骤边界被取消：detail 已带中断原因（如"等待被取消"），
		// 单独标 cancelled 让"人为中止"与"升级失败"在 runs 列表可区分
		status = "cancelled"
		summary = "用户取消：" + res.Detail
	}
	s.finishUpgradeRun(runID, h, status, summary, user, ip)
}

// finishUpgradeRun 终态收尾：落库 + SSE 事件 + 审计（用户/IP 捕获自受理
// 请求——后台 goroutine 收尾时请求早已返回，不能再走 s.audit(r,…)）。
func (s *Server) finishUpgradeRun(runID int64, h *store.Host, status, summary, user, ip string) {
	if err := s.st.FinishRun(runID, status, summary); err != nil {
		s.logger.Warn("finish upgrade run failed", "run_id", runID, "err", err)
	}
	s.runs.notify(runEvent{ID: runID, Status: status, Summary: summary})
	action := "upgrade"
	if status != "succeeded" {
		action = "upgrade_failed"
	}
	s.auditEntry(user, ip, action, "agent", h.Name, summary)
	s.logger.Info("agent upgrade run finished", "run_id", runID, "host", h.Name, "status", status)
}

// handleUpgradeBatch 批量升级（POST /api/hosts/upgrade {"ids":[],"force":bool}）：
// 每台主机一条独立 upgrade run（受理即全部建好、返回 run_id 列表），后台
// 有界并发 3 推进。此前同步等全部完成——百台级 30 分钟以上的挂起请求必然
// 被反代/浏览器掐断，断连后进度不可恢复。
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
	// 保序去重：同一台的重复 run 会被闸门串行化成"升级完再幂等短路"
	// 一轮空转（未定版本的开发构建还会重推二进制），提交端就该挡掉
	seen := make(map[int64]bool, len(req.IDs))
	uniq := make([]int64, 0, len(req.IDs))
	for _, id := range req.IDs {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	hosts := make([]*store.Host, 0, len(uniq))
	for _, id := range uniq {
		h, err := s.st.GetHost(id)
		// 台账已删的 ID 跳过（客户端可能持有过期列表）；但 DB 故障必须
		// fail-loud——静默缩小升级范围会让"部分主机没升级"看起来像成功
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			s.writeInternal(w, err)
			return
		}
		hosts = append(hosts, h)
	}
	if len(hosts) == 0 {
		writeError(w, http.StatusBadRequest, "no valid hosts")
		return
	}
	user, _ := r.Context().Value(ctxUser{}).(string)
	runIDs := make([]int64, 0, len(hosts))
	for _, h := range hosts {
		// 批量建 queued：worker 拿到主机闸门才转 running（与 app run 同
		// 语义，前端能区分"排队中/升级中"）
		runID, err := s.createUpgradeRun(h, "queued", user)
		if err != nil {
			// 中途 DB 故障：已建的 run 收口为 failed。ReconcileStaleRuns
			// 只在进程重启时兜底，不收口就留下无人推进的幽灵 running 行
			for _, rid := range runIDs {
				_ = s.st.FinishRun(rid, "failed", "run 创建中断（存储故障）")
			}
			s.writeInternal(w, err)
			return
		}
		runIDs = append(runIDs, runID)
	}
	s.audit(r, "batch_upgrade", "agent", fmt.Sprintf("%d 台", len(hosts)),
		fmt.Sprintf("已受理 %d 个升级 run（并发 3，逐台结果见各 run）", len(runIDs)))
	ip := s.remoteIP(r)
	go s.execUpgradeBatch(runIDs, hosts, req.Force, user, ip)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"run_ids": runIDs,
		"hosts":   upgradeRunRefs(runIDs, hosts),
	})
}

// upgradeRunRefs 批量受理响应的 run↔主机映射：跳过已删主机后 run_ids 与
// 请求 ids 不再对位，前端按它把 run 进度对回主机行。
func upgradeRunRefs(runIDs []int64, hosts []*store.Host) []map[string]any {
	out := make([]map[string]any, 0, len(hosts))
	for i, h := range hosts {
		out = append(out, map[string]any{"run_id": runIDs[i], "host_id": h.ID, "name": h.Name})
	}
	return out
}

// execUpgradeBatch 批量升级后台推进：并发 3（沿用既有节奏：每台含二进制
// 上传与重启等待，更高并发会同时打断更多主机的在途任务）。终态审计在各
// run 收尾时逐台落（finishUpgradeRun），这里不再聚合。
func (s *Server) execUpgradeBatch(runIDs []int64, hosts []*store.Host, force bool, user, ip string) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	for i := range hosts {
		wg.Add(1)
		go func(runID int64, h *store.Host) {
			// 取消注册在排队（信号量+闸门）前完成：排队期即可被
			// /api/runs/{id}/cancel 命中，轮到该 run 时直接跳过
			s.registerRunCancels([]int64{runID})
			defer s.unregisterRunCancel(runID)
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.execOneUpgradeRun(runID, h, force, user, ip)
		}(runIDs[i], hosts[i])
	}
	wg.Wait()
}

// execOneUpgradeRun 单台排队执行：等主机闸门（上限 runQueueTimeout，与
// app run 同口径——闸门被长执行占用时排队超时可失败可见，而不是无限
// 堆积等待中的 goroutine），拿到后转 running 并推进升级步骤。
func (s *Server) execOneUpgradeRun(runID int64, h *store.Host, force bool, user, ip string) {
	if s.runCancelRequested(runID) {
		s.finishUpgradeRun(runID, h, "cancelled", "用户取消（排队阶段，未执行任何步骤）", user, ip)
		return
	}
	queueCtx, cancelQ := context.WithTimeout(s.background(), runQueueTimeout)
	defer cancelQ()
	release, ok := s.gate.AcquireCtx(queueCtx, []int64{h.ID})
	if !ok {
		reason := "server 正在关停，升级排队中止"
		if queueCtx.Err() == context.DeadlineExceeded {
			reason = fmt.Sprintf("排队超时（%s 内未取得主机执行闸门）", runQueueTimeout)
		}
		s.finishUpgradeRun(runID, h, "failed", reason, user, ip)
		return
	}
	defer release()
	s.runUpgradeRun(runID, h, force, user, ip)
}

// upgradeAgent 执行单台升级全流程（阻塞；含最长 90s 的新版上线等待）。
// onStep 是进度回调（task/status/detail，status ∈ ok|failed）：每步边界
// 调用一次，由调用方决定落 run_tasks 还是忽略（传 nil 即可）。
func (s *Server) upgradeAgent(ctx context.Context, h *store.Host, force bool, onStep func(task, status, detail string)) UpgradeResult {
	res := UpgradeResult{ID: h.ID, Name: h.Name, To: agent.BuildVersion()}
	step := func(task, status, detail string) {
		if onStep != nil {
			onStep(task, status, detail)
		}
	}

	// 1. 探活：平台与当前版本
	pr := probeHost(ctx, h, s.probeClientFor(h))
	if pr.Status != "online" {
		res.Detail = "agent 不可达：" + pr.Error
		step("probe", "failed", res.Detail)
		return res
	}
	res.From = pr.Build
	platform := platformKey(pr.Goos, pr.Arch)
	if platform == "" {
		res.Detail = fmt.Sprintf("不支持的平台 %s/%s", pr.Goos, pr.Arch)
		step("probe", "failed", res.Detail)
		return res
	}
	step("probe", "ok", fmt.Sprintf("%s/%s · %s → %s", pr.Goos, pr.Arch, orUnknown(pr.Build), res.To))

	// 2. 版本相同且未强制 → 幂等成功（无需二进制参与，先于 resolver 判断）。
	//    未注入构建信息的开发构建（buildinfo.Unversioned）例外：版本串不
	//    反映二进制内容，同串不能证明同版本——不做短路，直接重推二进制
	if !force && pr.Build == res.To && !buildinfo.Unversioned() {
		res.OK = true
		res.Detail = "已是最新版本 " + res.To
		_ = s.st.SetHostStatus(h.ID, "online", pr.Build, "")
		return res
	}

	// 3. server 同级 bin 目录找目标平台二进制（与 enroll 推装同一来源）
	binPath, ok := s.binResolver(platform)
	if !ok {
		res.Detail = fmt.Sprintf("server 缺少 %s 二进制（须从 build.sh 产出的 bin 目录运行）", platform)
		step("binary", "failed", res.Detail)
		return res
	}
	step("binary", "ok", fmt.Sprintf("server 二进制就绪（%s）", platform))

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
	dc := &conn.Defaults{Conn: "agent"}
	ac := agentc.New(s.agentHostModel(h), dc)
	defer ac.Close()
	f, err := os.Open(binPath)
	if err != nil {
		res.Detail = "打开二进制失败: " + err.Error()
		step("binary", "failed", res.Detail)
		return res
	}
	defer f.Close()
	// 上传超时 5 分钟只封顶本次 UploadFile：uctx 不外传，后续 exec/探活
	// 等待仍挂外层 ctx，defer 释放不改变超时的生效范围
	uctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := ac.UploadFile(uctx, tmp, f, 0o755); err != nil {
		res.Detail = "上传失败: " + err.Error()
		step("upload", "failed", res.Detail)
		return res
	}
	step("upload", "ok", fmt.Sprintf("已上传 %s", tmp))

	// 5. 原子替换 + 重启。systemd restart 会杀掉 agent 自身 → 该 exec 的
	//    HTTP 响应大概率中断，错误是预期，转入探活等待。
	script := upgradeScript(tmp, dst)
	out, execErr := ac.Exec(ctx, conn.ExecRequest{Script: script, TimeoutMs: 30000, Label: "agent-upgrade"})
	manualRestart := execErr == nil && strings.Contains(out.Stdout, "WDP_NO_SYSTEMD")
	if manualRestart {
		step("replace", "ok", "已替换二进制；agent 非 systemd 托管（WDP_NO_SYSTEMD）")
	} else if execErr != nil {
		// 响应中断不在此判 failed：无法区分"agent 重启中"与"脚本真失败"，
		// 成败由下一步探活等待裁决（与旧同步实现同口径）
		step("replace", "ok", "替换/重启命令无响应（agent 重启中属预期）："+execErr.Error())
	} else {
		step("replace", "ok", strings.TrimSpace(out.Stdout))
	}

	// 6. 等待新版上线（systemd restart 后服务需数秒拉起）
	if !manualRestart {
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if ctx.Err() != nil {
				res.Detail = "等待被取消"
				step("wait-online", "failed", res.Detail)
				return res
			}
			time.Sleep(3 * time.Second)
			p2 := probeHost(ctx, h, s.probeClientFor(h))
			if p2.Status == "online" {
				if p2.Build == res.To {
					res.OK = true
					res.Detail = fmt.Sprintf("%s → %s，已重启上线", orUnknown(res.From), res.To)
					// 即时回写：升级按钮的「已是最新」门控不等下一轮探活
					_ = s.st.SetHostStatus(h.ID, "online", p2.Build, "")
					step("wait-online", "ok", res.Detail)
					return res
				}
				// 上线了但还是旧 build：二进制没换成功（路径不对/权限）
				res.Detail = fmt.Sprintf("agent 已重启但版本仍为 %s（二进制替换未生效，检查安装路径）", orUnknown(p2.Build))
				step("wait-online", "failed", res.Detail)
				return res
			}
		}
		res.Detail = "替换已下发但 agent 未在 90s 内恢复（systemd 拉起失败？在目标机查 systemctl status wdp-agent）"
		step("wait-online", "failed", res.Detail)
		return res
	}
	// 非 systemd：二进制已就位，旧进程还在跑旧代码
	res.OK = true
	res.Detail = fmt.Sprintf("二进制已更新到 %s；该 agent 非 systemd 托管，需手动重启进程生效", res.To)
	return res
}

func orUnknown(v string) string {
	if v == "" {
		return "(旧版)"
	}
	return v
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
