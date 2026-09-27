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

// run 状态 → el-tag type（succeeded 绿 / failed 红 / queued 黄 / 其余如
// running 灰）
export function runStatus(s: string): 'success' | 'danger' | 'info' | 'warning' {
  return s === 'succeeded' ? 'success' : s === 'failed' ? 'danger' : s === 'queued' ? 'warning' : 'info'
}
