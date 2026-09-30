package web

// 控制台运行时设置：单文档（settings 表 id=1 唯一行）覆盖一批原先是
// 启动 flag 的行为开关。目标形态：启动只配「这个东西在哪」（监听地址、
// 数据目录、数据库文件、TLS 材料路径——库地址由 --db 显式指定，缺省
// <data>/wdp.db），行为参数（探活周期、会话时效、告警阈值、保留策略、
// 明文纳管）进设置页在线改、即时生效；flag 保留为首启默认。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"wdp/internal/store"
)

// settingsDoc 设置文档。全部字段指针语义：nil = 未设置（用默认），显式
// 零值合法（如保留 0=永久）——区分「没配」与「配了 0」。
type settingsDoc struct {
	ProbeEverySec        *int64 `json:"probe_every_sec,omitempty"`
	SessionTTLMin        *int64 `json:"session_ttl_min,omitempty"`
	AlertWarnPct         *int   `json:"alert_warn_pct,omitempty"`
	AlertCritPct         *int   `json:"alert_crit_pct,omitempty"`
	MetricsRetainDays    *int   `json:"metrics_retain_days,omitempty"`
	RunsRetentionDays    *int   `json:"runs_retention_days,omitempty"`
	AuditRetentionDays   *int   `json:"audit_retention_days,omitempty"`
	DraftsRetentionDays  *int   `json:"drafts_retention_days,omitempty"`
	AllowPlaintextEnroll *bool  `json:"allow_plaintext_enroll,omitempty"`
}

// settingsView GET 响应：文档 + 元信息（版本/更新人）。
type settingsView struct {
	settingsDoc
	Version   int    `json:"version"`
	UpdatedAt string `json:"updated_at,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
}

// liveSettings 生效值缓存：读热路径（每请求的明文闸门、每次登录的 TTL）
// 无锁取快照；写路径整文档替换（atomic.Pointer 天然单份）。mu 只用于
// 序列化并发保存（版本乐观锁在 store 层，这里挡读-改-写窗口）。
type liveSettings struct {
	doc atomic.Pointer[settingsDoc]
	mu  sync.Mutex
}

// defaultSettings 默认设置（nil 字段一律回落到 flag/常量缺省）。
func defaultSettings() *settingsDoc { return &settingsDoc{} }

// loadSettingsInto 启动时把库内设置加载进生效缓存；没有行 = 全默认。
// 坏文档不当致命错：按默认跑并留日志。
func (s *Server) loadSettingsInto() error {
	row, err := s.st.LoadSettings()
	if errors.Is(err, store.ErrNotFound) {
		s.settings.doc.Store(defaultSettings())
		return nil
	}
	if err != nil {
		return err
	}
	var d settingsDoc
	if err := json.Unmarshal([]byte(row.Data), &d); err != nil {
		s.logger.Error("settings doc corrupt, falling back to defaults", "err", err, "raw_len", len(row.Data))
		d = settingsDoc{}
	}
	s.settings.doc.Store(&d)
	return nil
}

// effectiveSettings 当前生效文档快照（永不 nil）。
func (s *Server) effectiveSettings() *settingsDoc {
	if d := s.settings.doc.Load(); d != nil {
		return d
	}
	return defaultSettings()
}

// validateSettings 校验并归一化（非法值返回错误文案）。返回的副本用于
// 落库，不回写调用方。
func (s *Server) validateSettings(d *settingsDoc) (*settingsDoc, string) {
	out := settingsDoc{}
	if d.ProbeEverySec != nil {
		if *d.ProbeEverySec < 5 || *d.ProbeEverySec > 3600 {
			return nil, "probe_every_sec 范围 5–3600 秒"
		}
		v := *d.ProbeEverySec
		out.ProbeEverySec = &v
	}
	if d.SessionTTLMin != nil {
		if *d.SessionTTLMin < 5 || *d.SessionTTLMin > 24*60 {
			return nil, "session_ttl_min 范围 5–1440 分钟"
		}
		v := *d.SessionTTLMin
		out.SessionTTLMin = &v
	}
	if d.AlertWarnPct != nil {
		if *d.AlertWarnPct < 50 || *d.AlertWarnPct > 99 {
			return nil, "alert_warn_pct 范围 50–99"
		}
		v := *d.AlertWarnPct
		out.AlertWarnPct = &v
	}
	if d.AlertCritPct != nil {
		warn := 85
		if out.AlertWarnPct != nil {
			warn = *out.AlertWarnPct
		}
		if *d.AlertCritPct <= warn || *d.AlertCritPct > 100 {
			return nil, "alert_crit_pct 必须大于 warn 阈值且 ≤100"
		}
		v := *d.AlertCritPct
		out.AlertCritPct = &v
	}
	if d.MetricsRetainDays != nil {
		if *d.MetricsRetainDays < 1 || *d.MetricsRetainDays > 3650 {
			return nil, "metrics_retain_days 范围 1–3650 天"
		}
		v := *d.MetricsRetainDays
		out.MetricsRetainDays = &v
	}
	for _, it := range []struct {
		v   *int
		dst **int
		n   string
	}{
		{d.RunsRetentionDays, &out.RunsRetentionDays, "runs_retention_days"},
		{d.AuditRetentionDays, &out.AuditRetentionDays, "audit_retention_days"},
		{d.DraftsRetentionDays, &out.DraftsRetentionDays, "drafts_retention_days"},
	} {
		if it.v == nil {
			continue
		}
		if *it.v < 0 || *it.v > 3650 {
			return nil, it.n + " 范围 0–3650 天（0 = 永久保留）"
		}
		v := *it.v
		*it.dst = &v
	}
	if d.AllowPlaintextEnroll != nil {
		v := *d.AllowPlaintextEnroll
		out.AllowPlaintextEnroll = &v
	}
	return &out, ""
}

// handleGetSettings GET /api/settings（admin）。
func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	row, err := s.st.LoadSettings()
	view := settingsView{}
	switch {
	case errors.Is(err, store.ErrNotFound):
		// 从未配置：全默认
	case err != nil:
		s.writeInternal(w, err)
		return
	default:
		var d settingsDoc
		if json.Unmarshal([]byte(row.Data), &d) == nil {
			view.settingsDoc = d
		}
		view.Version = row.Version
		view.UpdatedAt = row.UpdatedAt
		view.UpdatedBy = row.UpdatedBy
	}
	writeJSON(w, http.StatusOK, view)
}

// handlePutSettings PUT /api/settings（admin）：整文档保存（带期望版本，
// 局部字段省略 = 沿用现值）。
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Settings settingsDoc `json:"settings"`
		Version  int         `json:"version"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	norm, errMsg := s.validateSettings(&req.Settings)
	if norm == nil {
		writeError(w, http.StatusBadRequest, errMsg)
		return
	}

	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()

	merged := mergeSettings(s.effectiveSettings(), norm)
	data, err := json.Marshal(merged)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	expect := req.Version
	if _, lerr := s.st.LoadSettings(); errors.Is(lerr, store.ErrNotFound) && expect == 0 {
		expect = -1 // 首配：行不存在时 0 版期望会被当成「已存在且版本不符」
	}
	newVer, serr := s.st.SaveSettings(string(data), expect, userFromReq(r))
	if errors.Is(serr, store.ErrSettingsVersion) {
		writeError(w, http.StatusConflict, "设置已被他人修改，请刷新后重试")
		return
	}
	if serr != nil {
		s.writeInternal(w, serr)
		return
	}

	// 即时生效：整份替换缓存；周期类 worker 按新周期重启
	s.settings.doc.Store(merged)
	s.applyLiveSettings()

	s.audit(r, "update", "settings", "console", "版本 "+strconv.Itoa(newVer))
	writeJSON(w, http.StatusOK, settingsView{
		settingsDoc: *merged,
		Version:     newVer,
	})
}

// mergeSettings 局部保存合并：next 里 nil 的字段沿用 cur。
func mergeSettings(cur, next *settingsDoc) *settingsDoc {
	out := *next
	if out.ProbeEverySec == nil {
		out.ProbeEverySec = cur.ProbeEverySec
	}
	if out.SessionTTLMin == nil {
		out.SessionTTLMin = cur.SessionTTLMin
	}
	if out.AlertWarnPct == nil {
		out.AlertWarnPct = cur.AlertWarnPct
	}
	if out.AlertCritPct == nil {
		out.AlertCritPct = cur.AlertCritPct
	}
	if out.MetricsRetainDays == nil {
		out.MetricsRetainDays = cur.MetricsRetainDays
	}
	if out.RunsRetentionDays == nil {
		out.RunsRetentionDays = cur.RunsRetentionDays
	}
	if out.AuditRetentionDays == nil {
		out.AuditRetentionDays = cur.AuditRetentionDays
	}
	if out.DraftsRetentionDays == nil {
		out.DraftsRetentionDays = cur.DraftsRetentionDays
	}
	if out.AllowPlaintextEnroll == nil {
		out.AllowPlaintextEnroll = cur.AllowPlaintextEnroll
	}
	return &out
}

// userFromReq 会话用户名（审计口径；无会话时 system）。
func userFromReq(r *http.Request) string {
	if u, ok := r.Context().Value(ctxUser{}).(string); ok && u != "" {
		return u
	}
	return "system"
}

// ---- 生效接线 ----

// applyLiveSettings 把当前设置文档接到各消费点：会话窗口/告警阈值/
// 指标保留即时生效；探活周期在 Run 生命周期内停旧起新；保留清理异步
// 补一轮。启动（New 尾声）与每次保存都会走到这里。
func (s *Server) applyLiveSettings() {
	d := s.effectiveSettings()
	if d.SessionTTLMin != nil {
		s.sessions.idleNS.Store(int64(time.Duration(*d.SessionTTLMin) * time.Minute))
	} else {
		s.sessions.idleNS.Store(0)
	}
	doc := d // 闭包持快照：字段引用不随后续保存漂移
	s.monitor.Thresholds = func() (int, int) {
		if doc.AlertWarnPct != nil && doc.AlertCritPct != nil {
			return *doc.AlertWarnPct, *doc.AlertCritPct
		}
		return 0, 0 // worker 回落内置默认
	}
	s.monitor.RetainDays = func() int {
		if doc.MetricsRetainDays != nil {
			return *doc.MetricsRetainDays
		}
		return 0 // 同上
	}
	if s.bgCtx != nil {
		s.startProber(s.bgCtx)
		go s.retentionOnce()
	}
}

// allowPlaintextEnroll 明文纳管总闸：设置页开关优先，flag 为首启默认
// （admin 在设置页显式关闭后，带 flag 的旧启动脚本也不再放行）。
func (s *Server) allowPlaintextEnroll() bool {
	if b := s.effectiveSettings().AllowPlaintextEnroll; b != nil {
		return *b
	}
	return s.opts.AllowPlaintextEnroll
}

// 生效保留天数（设置值优先，回落 flag）。
func (s *Server) runsRetentionDays() int {
	if d := s.effectiveSettings().RunsRetentionDays; d != nil {
		return *d
	}
	return s.opts.RunsRetentionDays
}

func (s *Server) auditRetentionDays() int {
	if d := s.effectiveSettings().AuditRetentionDays; d != nil {
		return *d
	}
	return s.opts.AuditRetentionDays
}

func (s *Server) draftsRetentionDays() int {
	if d := s.effectiveSettings().DraftsRetentionDays; d != nil {
		return *d
	}
	return s.opts.DraftsRetentionDays
}
