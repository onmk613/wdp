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

	"wdp/internal/store"
)

// ---- 工具 ----

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid id")
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

// decodeJSONLimit 带上限的 JSON 解码（decodeJSON/decodeJSONLarge 的公共
// 实现）。超限 413 与 JSON 语法错 400 分流：客户端能凭状态码区分"改小
// 请求体"与"修 JSON"。
func decodeJSONLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
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
	if dec.More() {
		writeError(w, http.StatusBadRequest, "invalid JSON body: trailing data after JSON value")
		return false
	}
	return true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeJSONLimit(w, r, v, 1<<20)
}

// specPayloadLimit 编辑器保存/校验请求体上限（全量文本文件 + JSON 包装）。
// 必须与草稿上限（draftPayloadLimit）同口径：草稿能存下的内容保存不了
// 会造成「暂存成功、正式保存永久失败」的死局；全局 decodeJSON 的 1MiB
// 对多文件 chart 不够（tgz 上传路径允许的包更大）。
const specPayloadLimit = 8 << 20

// decodeJSONLarge 同 decodeJSON，用于编辑器保存/校验的大请求体（上限
// specPayloadLimit + 64KiB 余量），超限回 413（区别于 JSON 语法错 400）。
func decodeJSONLarge(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeJSONLimit(w, r, v, specPayloadLimit+64<<10)
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
//
// 执行类任务（exec/升级/SSH 推装）一律挂它而非 r.Context()：直连请求
// ctx 时，用户关标签页/网络抖动 → ctx 取消 → agent 端对进程组 SIGKILL，
// 远端停在半完成状态——脚本执行到一半、目标机残留 .wdp-upgrade-* 临时
// 文件甚至二进制已换未重启、agent 装到一半——对变更类操作是数据损坏。
// 代价是断连后响应写往死连接（无害），执行结果仍完整落在 run 记录或由
// 后续探活兜底。
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

// writeStoreErr store 错误→HTTP 的统一分流：操作对象不存在（ErrNotFound）
// 回 404；业务校验文案（store.IsBizErr，含"已存在"与必填/格式类）回 400
// 原样透出；其余（数据库引擎/IO 内部错误）走 writeInternal 脱敏——裸
// SQL 错误串会暴露内部表结构与路径。store 调用的错误映射一律走这里，
// handler 不再自行判定状态码：散落各处的手写分流曾把 DB 故障统一回成
// 404/400，把基础设施故障伪装成"对象不存在/用户输入错"。
func (s *Server) writeStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if store.IsBizErr(err) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeInternal(w, err)
}

// replyApp 入库成功后的应用回读响应（GetApp 错误分流内置）：失败不再
// 吞错回 JSON null——ErrNotFound 404、DB 故障 500（writeStoreErr 口径）。
// code 为成功时的响应状态（创建路径 201，其余 200）。
func (s *Server) replyApp(w http.ResponseWriter, id int64, code int) {
	app, err := s.st.GetApp(id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, code, app)
}
