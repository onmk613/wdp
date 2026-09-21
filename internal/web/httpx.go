package web

// HTTP 传输工具：路径参数解析、JSON 解码（普通/大请求体）、统一响应
// 口径（writeJSON/writeError/writeInternal）、后台生命周期 ctx。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// ---- 工具 ----

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid host id")
		return 0, false
	}
	return id, true
}

// path2ID 解析两级数字路径参数（/api/apps/{id}/versions/{vid}）。
func path2ID(w http.ResponseWriter, r *http.Request, idKey, subKey string) (int64, int64, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return 0, 0, false
	}
	sub, err := strconv.ParseInt(r.PathValue(subKey), 10, 64)
	if err != nil || sub <= 0 {
		writeError(w, http.StatusBadRequest, "invalid "+subKey)
		return 0, 0, false
	}
	return id, sub, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// specPayloadLimit 编辑器保存/校验请求体上限（全量文本文件 + JSON 包装）。
// 必须与草稿上限（draftPayloadLimit）同口径：草稿能存下的内容保存不了
// 会造成「暂存成功、正式保存永久失败」的死局；全局 decodeJSON 的 1MiB
// 对多文件 chart 不够（tgz 上传路径允许的包更大）。
const specPayloadLimit = 8 << 20

// decodeJSONLarge 同 decodeJSON，用于编辑器保存/校验的大请求体，超限回
// 413（区别于 JSON 语法错 400）。
func decodeJSONLarge(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, specPayloadLimit+64<<10))
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("payload exceeds %d bytes limit", mbe.Limit))
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// background 返回后台任务应挂钩的生命周期 ctx（Run 启动后随 server 关停
// 取消；测试直连 Handler 未注入时退回 Background）。
func (s *Server) background() context.Context {
	if s.bgCtx != nil {
		return s.bgCtx
	}
	return context.Background()
}

// writeInternal 500 统一口径：完整错误进服务端日志，客户端只收固定文案——
// err.Error() 常带文件路径/DB 细节，直接透出会暴露内部结构。
func (s *Server) writeInternal(w http.ResponseWriter, err error) {
	s.logger.Error("internal error", "err", err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}
