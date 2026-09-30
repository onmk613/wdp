// 展示层共用的小工具：各列表页重复出现的「字符串 → 展示形态」换算
//（标签 JSON 解析、状态文案 → el-tag 颜色），收敛到一处避免多份拷贝
// 悄悄漂移。函数均为纯函数，输出值即历史各页内联实现的逐字保持。

// 主机/应用 Labels 字段是 JSON 字符串：解析失败（空串/坏 JSON）回退空
// 对象——展示层永远拿到可遍历的 map，不往外抛异常
export function parseLabels(raw: string): Record<string, string> {
  try {
    return JSON.parse(raw || '{}')
  } catch {
    return {}
  }
}

// 主机在线状态 → el-tag type（online 绿 / offline 红 / 其余如 unknown 灰）
export function statusType(s: string | undefined): 'success' | 'danger' | 'info' {
  return s === 'online' ? 'success' : s === 'offline' ? 'danger' : 'info'
}

// 任务状态 → el-tag type（ok 绿 / skipped 灰 / 其余 failed 红）
export function taskStatus(s: string): 'success' | 'info' | 'danger' {
  return s === 'ok' ? 'success' : s === 'skipped' ? 'info' : 'danger'
}

// run 状态 → el-tag type（succeeded 绿 / failed 红 / queued 黄 / cancelled
// 灰 / 其余如 running 灰）
export function runStatus(s: string): 'success' | 'danger' | 'info' | 'warning' {
  return s === 'succeeded'
    ? 'success'
    : s === 'failed' ? 'danger'
      : s === 'queued' || s === 'cancelling' ? 'warning'
        : 'info'
}

// 统一时间展示：后端一律 RFC3339 UTC 裸串，直接展示既不符中文习惯还
// 三种格式混用（ISO / 美式 locale / +08 偏移）。统一 zh-CN 24 小时制；
// 空值回退 '-'，解析失败原样透出（不吞数据）
export function fmtTime(iso: string | undefined | null): string {
  if (!iso) return '-'
  const d = new Date(iso)
  return isNaN(d.getTime()) ? iso : d.toLocaleString('zh-CN', { hour12: false })
}

// run 目标选择器（内部 JSON）→ 可读文案：{"ids":[1],"kind":"hosts"} →
// 「主机 #1」；解析失败原样返回（不比裸 JSON 更差）
export function selectorLabel(raw: string | undefined | null): string {
  if (!raw) return '-'
  try {
    const sel = JSON.parse(raw) as { ids?: number[]; names?: string[]; hosts?: string[]; kind?: string }
    const kind = sel.kind || 'hosts'
    const kindName = kind === 'hosts' ? '主机' : kind === 'inline' ? '自定义' : kind === 'pools' ? '池' : kind === 'groups' ? '组' : kind === 'labels' ? '标签' : kind
    const items = (sel.hosts || sel.names || sel.ids || []).map((v) => (typeof v === 'number' ? `#${v}` : v))
    return items.length ? `${kindName} ${items.join('、')}` : kindName
  } catch {
    return raw
  }
}
