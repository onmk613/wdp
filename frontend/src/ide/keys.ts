// IDE 键集合的单一来源：play 级键 / 任务控制键。此前 AppIDE、
// PlaybookPage、complete.ts 各持一份拷贝，控制键的剔除清单一旦调整
// 就会出现编辑器内着色/补全口径不一致——收敛于此（控制键本体仍定义在
// schemas.ts，它由 /api/schema 派生；此处仅转发，保证一个定义点）。

import type { SchemaMeta } from '../api'
import { controlKeys } from './schemas'

export { controlKeys }

// play 级键：schema play 段全部字段 + tasks/handlers 两个列表容器键
//（容器键由引擎固定识别，不在 schema 字段表里）。meta 未加载时返回
// 空集合（调用方以此走「无上下文」分支）
export function playKeysSet(meta: SchemaMeta | null): Set<string> {
  const s = new Set<string>()
  if (!meta) return s
  for (const sec of meta.play) for (const f of sec.fields) s.add(f.name)
  s.add('tasks')
  s.add('handlers')
  return s
}
