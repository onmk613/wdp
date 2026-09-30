package web

// 列表分页的统一约定（hosts/apps/runs/audit 全走这一套）：
//
//	GET /api/<res>?page=1&page_size=50&q=…
//
// 响应信封：
//
//	{"items": [...], "total": 1234, "page": 1, "page_size": 50}
//
// 规则：
//   - page 从 1 起；越界页返回空 items（total 仍真实，前端据此修页码）
//   - page_size 缺省 50，上限 500（防一次拖全表）；0/负数归一为缺省
//   - q 搜索在**全量数据**上过滤后分页——搜索语义面向全部条目，
//     不是当前页；这是 store 层把 WHERE 与 LIMIT 放同一条 SQL 的原因
//   - total 是过滤后总数（搜索命中多少条就报多少），翻页/跳页都以它为准
//
// 兼容：不带分页参数的老调用方（CSV 导入预检、SSE、exec 主机圈选等
// 服务端内部调用）仍要拿全量——store 层保留原 List* 全量函数，分页
// 版本是新增的 List*Page。

import (
	"net/http"
	"strconv"
)

// pageParams 解析 ?page=&page_size=。
type pageParams struct {
	Page     int
	PageSize int
}

func parsePage(r *http.Request) pageParams {
	q := r.URL.Query()
	p := pageParams{Page: 1, PageSize: 50}
	if v, err := strconv.Atoi(q.Get("page")); err == nil && v > 0 {
		p.Page = v
	}
	if v, err := strconv.Atoi(q.Get("page_size")); err == nil && v > 0 {
		p.PageSize = v
	}
	if p.PageSize > 500 {
		p.PageSize = 500
	}
	return p
}

// offset LIMIT/OFFSET 的偏移。
func (p pageParams) offset() int { return (p.Page - 1) * p.PageSize }

// pagedResp 分页信封。items 由调用方填充（泛型信封在 Go 1.27 可用，
// 但各资源类型不同，这里用 any 保持 handler 内类型安全）。
type pagedResp[T any] struct {
	Items    []T   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}
