package web

// 应用执行的自定义目标：selector.kind = "inline" + 粘贴的 inventory
// YAML——针对未纳管主机（conn: ssh 直连或自带证书材料的 agent）执行应用，
// 与「已纳管主机选择器」二选一。解析出的主机模型直通 executor（不经
// 台账/探活），inventory 文本（可能含凭据）不落库：run 的 selector 只记
// 主机名，执行期内存即弃。

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"

	"wdp/internal/inventory"
	"wdp/internal/model"
)

const (
	// maxInlineInventoryBytes 粘贴 inventory 的尺寸上限（防误粘整本清单文件）。
	maxInlineInventoryBytes = 256 << 10
	// maxInlineHosts 单次自定义执行的主机数上限（与内联清单区间展开同档）。
	maxInlineHosts = 256
)

// runAppsInlineTargets 解析 inline 选择器：全局 run:execute 专用——
// 作用域受限用户对未纳管主机无法界定范围，粘贴 inventory 会绕开主机
// 作用域模型，直接拒绝。
func (s *Server) runAppsInlineTargets(w http.ResponseWriter, r *http.Request, text string) ([]*model.Host, string, bool) {
	if s.hostScopeSet(r, verbRunExec) != nil {
		writeError(w, http.StatusForbidden,
			"forbidden: 自定义目标（粘贴 inventory）要求全局 run:execute 权限（主机作用域无法覆盖未纳管主机）")
		return nil, "", false
	}
	if text == "" {
		writeError(w, http.StatusBadRequest, "inline selector requires pasted inventory text")
		return nil, "", false
	}
	if len(text) > maxInlineInventoryBytes {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("inventory text too large (%d bytes, max %d)", len(text), maxInlineInventoryBytes))
		return nil, "", false
	}
	inv, err := inventory.Parse([]byte(text))
	if err != nil {
		writeError(w, http.StatusBadRequest, "inventory 解析失败："+err.Error())
		return nil, "", false
	}
	if len(inv.Hosts) == 0 {
		writeError(w, http.StatusBadRequest, "inventory 未包含任何主机（需要 hosts: 段）")
		return nil, "", false
	}
	if len(inv.Hosts) > maxInlineHosts {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("inventory 主机数 %d 超过上限 %d", len(inv.Hosts), maxInlineHosts))
		return nil, "", false
	}
	names := make([]string, 0, len(inv.Hosts))
	for _, h := range inv.Hosts {
		names = append(names, h.Name)
	}
	sel, _ := json.Marshal(map[string]any{"kind": "inline", "hosts": names})
	s.audit(r, "run", "exec", "", fmt.Sprintf("自定义目标执行：%d 台未纳管主机", len(inv.Hosts)))
	return inv.Hosts, string(sel), true
}

// inlineGateIDs 未纳管主机的执行闸门键：台账闸门按主机 ID 加锁，自定义
// 主机没有 ID——取主机名 FNV 哈希的负值（与任何台账 ID 空间隔离；同名
// 主机跨 run 串行，不同名碰撞只是过度串行，无正确性影响）。
func inlineGateIDs(hosts []*model.Host) []int64 {
	ids := make([]int64, 0, len(hosts))
	for _, h := range hosts {
		f := fnv.New32a()
		_, _ = f.Write([]byte(h.Name))
		ids = append(ids, -int64(f.Sum32()|1)) // |1 保证非零
	}
	return ids
}
