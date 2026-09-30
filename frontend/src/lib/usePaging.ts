// 列表页分页状态（与后端 pagedResp 信封对齐）：
//   page/pageSize/total + load 回调（搜索/翻页/换页大小都触发）
//   搜索变更回到第 1 页；页码越界（total 变小）自动钳制。
// 搜索面向全量数据（后端 q 过滤后分页），这里不需要额外处理。
import { ref, watch } from 'vue'

export interface PagedResp<T> {
  items: T[]
  total: number
  page: number
  page_size: number
}

export function usePaging<T>(fetchPage: (page: number, pageSize: number) => Promise<PagedResp<T> | null>) {
  const items = ref<T[]>([]) as { value: T[] }
  const total = ref(0)
  const page = ref(1)
  const pageSize = ref(50)
  const loading = ref(false)

  async function load() {
    loading.value = true
    try {
      const r = await fetchPage(page.value, pageSize.value)
      if (r) {
        items.value = r.items
        total.value = r.total
        // 越界钳制：删除末页数据后当前页可能超界（后端返回空 items）
        if (page.value > 1 && r.items.length === 0 && r.total > 0) {
          page.value = Math.max(1, Math.ceil(r.total / pageSize.value))
          return load()
        }
      }
    } finally {
      loading.value = false
    }
  }

  function resetToFirst() {
    page.value = 1
    return load()
  }

  watch(pageSize, () => { /* 页码换算由 Pager 组件完成，这里只观察 */ })

  return { items, total, page, pageSize, loading, load, resetToFirst }
}
