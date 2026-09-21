package agent

import (
	"encoding/json"
	"net/http"
)

// writeJSON 输出 JSON HTTP 响应（handler 共用）。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
