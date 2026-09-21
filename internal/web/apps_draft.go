package web

// 编辑器草稿端点（按用户隔离）：自动暂存的载体。key 规则：
//   - 编辑模式："<appID>"（权限：GET 用 app:view，PUT/DELETE 用 app:edit）
//   - 新建模式："new:<应用名>"（权限 app:create）
//
// 草稿高频写入（前端 debounce 自动暂存），不进审计日志；payload 上限
// 8MiB——spec 全量文本文件受单文件 512KiB 限制，正常 chart 远达不到，
// 超限说明调用方异常（如误把二进制塞进来），拒绝而非截断。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"wdp/internal/store"
)

const draftPayloadLimit = 8 << 20

// draftKey 解析 ?key= 并做权限校验，返回草稿键。editVerb 是编辑模式下的
// 权限点（GET 传 app:view，写操作传 app:edit）；新建模式恒为 app:create。
// allowOrphan：编辑模式的底本应用已删除时仍放行（草稿箱删除孤儿草稿用——
// 应用没了，草稿是用户自己的，没有可校验的权限对象）。
func (s *Server) draftKey(w http.ResponseWriter, r *http.Request, editVerb string, allowOrphan bool) (string, bool) {
	raw := r.URL.Query().Get("key")
	if raw == "" {
		writeError(w, http.StatusBadRequest, "missing ?key= (<appID> or new:<name>)")
		return "", false
	}
	if strings.HasPrefix(raw, "new:") {
		name := strings.TrimPrefix(raw, "new:")
		if name == "" || !appNameRe.MatchString(name) {
			writeError(w, http.StatusBadRequest, "invalid new-app key (new:<name>)")
			return "", false
		}
		if !s.permsOf(permUser(r)).canVerb(verbAppCreate) {
			writeError(w, http.StatusForbidden, "forbidden: requires "+verbAppCreate)
			return "", false
		}
		return raw, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid draft key (must be <appID> or new:<name>)")
		return "", false
	}
	if allowOrphan {
		if _, err := s.st.GetApp(id); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.writeInternal(w, err)
			return "", false
		} else if errors.Is(err, store.ErrNotFound) {
			return raw, true // 应用已删除：孤儿草稿，仅允许清理
		}
	}
	if _, ok := s.checkApp(w, r, editVerb, id); !ok {
		return "", false
	}
	return raw, true
}

// draftUser 草稿按用户隔离：用户名 → users.id。
func (s *Server) draftUser(w http.ResponseWriter, r *http.Request) (int64, bool) {
	u, err := s.st.UserByName(permUser(r))
	if err != nil {
		s.writeInternal(w, err)
		return 0, false
	}
	return u.ID, true
}

// handleListDrafts GET /api/apps/drafts — 当前用户的草稿箱列表。key 形如
// new:<name>（新建中）或 <appID>（编辑中）；<appID> 项解析应用名，底本
// 应用已删除的标记 app_gone（草稿只剩清理价值）。权限：app:view 或
// app:create 任一（菜单入口两个角色都可见）。
func (s *Server) handleListDrafts(w http.ResponseWriter, r *http.Request) {
	p := s.permsOf(permUser(r))
	if !p.canVerb(verbAppView) && !p.canVerb(verbAppCreate) {
		writeError(w, http.StatusForbidden, "forbidden: requires "+verbAppView+" or "+verbAppCreate)
		return
	}
	uid, ok := s.draftUser(w, r)
	if !ok {
		return
	}
	drafts, err := s.st.ListAppDrafts(uid)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	type draftItem struct {
		Key         string `json:"key"`
		Kind        string `json:"kind"` // new = 新建中 / edit = 编辑既有应用
		AppName     string `json:"app_name"`
		AppGone     bool   `json:"app_gone"` // 底本应用已删除（草稿仅可清理）
		BaseVersion string `json:"base_version"`
		UpdatedAt   string `json:"updated_at"`
	}
	out := []draftItem{}
	for _, d := range drafts {
		it := draftItem{Key: d.AppKey, BaseVersion: d.BaseVersion, UpdatedAt: d.UpdatedAt}
		if name := strings.TrimPrefix(d.AppKey, "new:"); d.AppKey != name {
			it.Kind, it.AppName = "new", name
		} else if id, err := strconv.ParseInt(d.AppKey, 10, 64); err == nil {
			it.Kind = "edit"
			if app, err := s.st.GetApp(id); err == nil {
				it.AppName = app.Name
			} else {
				it.AppGone = true
			}
		} else {
			continue // 非法键：不进列表（历史脏数据不影响展示）
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetDraft GET /api/apps/draft?key=… — 读草稿（404 = 无草稿）。
func (s *Server) handleGetDraft(w http.ResponseWriter, r *http.Request) {
	key, ok := s.draftKey(w, r, verbAppView, false)
	if !ok {
		return
	}
	uid, ok := s.draftUser(w, r)
	if !ok {
		return
	}
	d, err := s.st.GetAppDraft(uid, key)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no draft")
		return
	}
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"payload": d.Payload, "base_version": d.BaseVersion, "updated_at": d.UpdatedAt,
	})
}

// handlePutDraft PUT /api/apps/draft — 写草稿（同键覆盖）。草稿是全量
// 文件快照，用独立的 8MiB 解码上限（decodeJSON 的全局 1MiB 对它不够），
// 超限回 413。
func (s *Server) handlePutDraft(w http.ResponseWriter, r *http.Request) {
	key, ok := s.draftKey(w, r, verbAppEdit, false)
	if !ok {
		return
	}
	uid, ok := s.draftUser(w, r)
	if !ok {
		return
	}
	var req struct {
		BaseVersion string `json:"base_version"`
		Payload     string `json:"payload"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, draftPayloadLimit+64<<10))
	if err := dec.Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("draft exceeds %d bytes limit", mbe.Limit))
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Payload) > draftPayloadLimit {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("draft payload exceeds %d bytes limit", draftPayloadLimit))
		return
	}
	if err := s.st.PutAppDraft(&store.AppDraft{
		UserID: uid, AppKey: key, BaseVersion: req.BaseVersion, Payload: req.Payload,
	}); err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeleteDraft DELETE /api/apps/draft?key=… — 清草稿（保存成功后调用）。
func (s *Server) handleDeleteDraft(w http.ResponseWriter, r *http.Request) {
	key, ok := s.draftKey(w, r, verbAppEdit, true)
	if !ok {
		return
	}
	uid, ok := s.draftUser(w, r)
	if !ok {
		return
	}
	if err := s.st.DeleteAppDraft(uid, key); err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
