package web

// 主机台账的基础 CRUD/探活端点（批量导入、纳管、升级等重操作在
// inventory_ops/enroll/sshinstall/upgrade 各自文件）。

import (
	"errors"
	"net/http"
	"wdp/internal/store"
)

// ---- 主机台账 ----

func (s *Server) handleListHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.st.ListHosts(r.URL.Query().Get("q"))
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.filterHosts(r, verbHostView, hosts))
}

func (s *Server) handleCreateHost(w http.ResponseWriter, r *http.Request) {
	var h store.Host
	if !decodeJSON(w, r, &h) {
		return
	}
	id, err := s.st.CreateHost(&h)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.ID = id
	s.audit(r, "create", "host", h.Name, h.Address)
	writeJSON(w, http.StatusCreated, h)
}

func (s *Server) handleGetHost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	h, ok := s.checkHost(w, r, verbHostView, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) handleUpdateHost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, ok := s.checkHost(w, r, verbHostEdit, id); !ok {
		return
	}
	var h store.Host
	if !decodeJSON(w, r, &h) {
		return
	}
	if err := s.st.UpdateHost(id, &h); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "host not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.st.GetHost(id)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	s.audit(r, "update", "host", updated.Name, "地址/端口/池/组/标签")
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	h, ok := s.checkHost(w, r, verbHostView, id)
	if !ok {
		return
	}
	result := probeHost(r.Context(), h, s.mtlsProbeClient())
	_ = s.st.SetHostStatus(h.ID, result.Status)
	writeJSON(w, http.StatusOK, result)
}
