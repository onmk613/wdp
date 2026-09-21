package web

// 操作审计：控制台的增删改动作统一落 audit_logs（谁在何时对什么做了
// 什么）。写入失败不阻断业务（仅记日志）；执行类动作不进审计表——
// runs 表已带触发者字段，两者互补：runs 答"执行了什么"，审计答"谁改
// 了什么"。

import (
	"fmt"
	"net/http"

	"wdp/internal/store"
)

// audit 写一条操作审计（用户取会话 context，IP 取 RemoteAddr 去端口）。
func (s *Server) audit(r *http.Request, action, object, name, detail string) {
	user, _ := r.Context().Value(ctxUser{}).(string)
	s.auditEntry(user, s.remoteIP(r), action, object, name, detail)
}

// auditEntry 底层写入（登录等无会话场景直接指定用户名）。
func (s *Server) auditEntry(user, ip, action, object, name, detail string) {
	if err := s.st.CreateAuditLog(&store.AuditLog{
		User: user, Action: action, Object: object, Name: name, Detail: detail, IP: ip,
	}); err != nil {
		s.logger.Warn("audit write failed", "action", action, "object", object, "err", err)
	}
}

// handleListAuditLogs 操作日志查询（?q= 模糊过滤，?limit=）。
func (s *Server) handleListAuditLogs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &limit)
	}
	logs, err := s.st.ListAuditLogs(limit, r.URL.Query().Get("q"))
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, logs)
}
