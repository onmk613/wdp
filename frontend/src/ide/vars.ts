// {{ }} 模板变量域：聚合 values.yaml、_helpers.tpl 的 define 名、当前
// chart 各 playbook 里的 register 变量、内置变量、模板函数白名单。
// 域随文件内容变化即时刷新（debounce 由调用方做）。

import yaml from 'js-yaml'
import type { SchemaMeta } from '../api'

export interface VarNode {
  name: string
  detail?: string // 来源说明（values / 内置 / register / item）
  children?: Record<string, VarNode> // 嵌套（values 子树）
}

export interface VarDomain {
  roots: Record<string, VarNode>
  helpers: string[] // {{ define "name" }}
  funcs: string[] // sprig 白名单 + 自有
}

function objToChildren(v: any, depth: number): Record<string, VarNode> | undefined {
  if (depth > 4 || v === null || typeof v !== 'object' || Array.isArray(v)) return undefined
  const out: Record<string, VarNode> = {}
  for (const [k, val] of Object.entries(v)) {
    out[k] = { name: k, children: objToChildren(val, depth + 1) }
  }
  return out
}

// buildVarDomain 从文件快照（path → content）聚合变量域。
export function buildVarDomain(files: Map<string, string>, meta: SchemaMeta): VarDomain {
  const roots: Record<string, VarNode> = {}
  const helpers: string[] = []

  for (const [path, content] of files) {
    if (path === 'values.yaml') {
      try {
        const v = yaml.load(content) as any
        if (v && typeof v === 'object' && !Array.isArray(v)) {
          for (const [k, val] of Object.entries(v)) {
            roots[k] = { name: k, detail: 'values.yaml', children: objToChildren(val, 1) }
          }
        }
      } catch { /* 语法错误时保留上一份域（调用方传入的就是上次成功解析的？不——直接跳过） */ }
    }
    if (path.endsWith('.tpl')) {
      for (const m of content.matchAll(/\{\{-?\s*define\s+"([^"]+)"/g)) helpers.push(m[1])
    }
    // 根目录相位 playbook：register 变量 + loop_var + handlers 名
    if (/^([A-Za-z0-9_-]+)\.yaml$/.test(path) && path !== 'chart.yaml' && path !== 'values.yaml' && path !== 'inventory.yaml') {
      for (const m of content.matchAll(/^\s*register:\s*([A-Za-z_][\w]*)\s*$/gm)) {
        if (!roots[m[1]]) roots[m[1]] = { name: m[1], detail: 'register' }
      }
      for (const m of content.matchAll(/^\s*loop_var:\s*([A-Za-z_][\w]*)\s*$/gm)) {
        if (!roots[m[1]]) roots[m[1]] = { name: m[1], detail: '循环变量' }
      }
    }
  }
  if (!roots['item']) roots['item'] = { name: 'item', detail: 'loop 循环项' }
  for (const b of meta.builtin_vars) {
    if (!roots[b]) roots[b] = { name: b, detail: '内置变量' }
  }
  return { roots, helpers, funcs: meta.template_funcs }
}

// resolvePathPrefix 把 '.app.po' 这类已输入前缀解析到最近的可续补节点：
// 返回 { parent: 未匹配完的最后完整段前的对象, rest: 正在输入的段, path: 完整段链 }
export function resolvePathPrefix(
  domain: VarDomain, typed: string,
): { node: VarNode; rest: string; chain: string[] } | null {
  // {{ 后的表达式带前导空格（"{{ .app."），先 trim 再剥前导点
  const clean = typed.trim().replace(/^\.+/, '')
  const endsDot = clean.endsWith('.')
  const segs = clean.split('.').filter(Boolean)
  if (!segs.length && !endsDot) return null
  // 尾点（.app.）= app 是完整段、正在输入下一级；无尾点则最后一段在输入中
  const typing = endsDot ? '' : (segs[segs.length - 1] || '')
  const walked = endsDot ? segs : segs.slice(0, -1)
  let node: VarNode = { name: '', children: domain.roots }
  for (const s of walked) {
    const next = node.children?.[s]
    if (!next) return null // 前缀已脱离已知域：不给补全（避免误导）
    node = next
  }
  return { node, rest: typing, chain: walked }
}

// ---- 值补全的数据源（hosts/notify/相位/子 chart/文件路径）----

// subchartNames charts/ 下的子 chart 名
export function subchartNames(paths: string[]): string[] {
  const out = new Set<string>()
  for (const p of paths) {
    const m = p.match(/^charts\/([^/]+)\//)
    if (m) out.add(m[1])
  }
  return [...out].sort()
}

// phaseNames 根目录相位（deploy/uninstall/自定义）+ 子 chart 相位
//（chart 引用的 tasks_from 补全用；prefix = 子 chart 名时列其相位）
export function phaseNames(paths: string[], subchart?: string): string[] {
  const out = new Set<string>()
  const base = subchart ? `charts/${subchart}/` : ''
  for (const p of paths) {
    if (!p.startsWith(base)) continue
    const rel = p.slice(base.length)
    if (rel.includes('/')) continue
    const m = rel.match(/^([A-Za-z0-9_-]+)\.yaml$/)
    if (!m) continue
    const n = m[1]
    if (['chart', 'values', 'inventory'].includes(n)) continue
    out.add(n)
  }
  return [...out].sort()
}

// handlerNames 收集 handler 名候选（control = 任务控制键集合）。两个来源：
//   · 根级 yaml 的 play handlers: 块（内联写法）
//   · handlers/ 目录约定片段（裸任务列表，每项一个 handler；play 里
//     `handlers: [- include: handlers/x.yaml]` 引入——与后端展开一致）
export function handlerNames(files: Map<string, string>, control: Set<string>): string[] {
  const out = new Set<string>()
  const nameOf = (h: unknown): string | null =>
    (h as any)?.name || (typeof h === 'object' ? Object.keys(h as object).find((k) => !control.has(k) && k !== 'name') || null : null)
  for (const [path, content] of files) {
    const isHandlerFragment = /^handlers\/[\w./-]+\.ya?ml$/.test(path)
    if (!isHandlerFragment) {
      if (!/^([A-Za-z0-9_-]+)\.yaml$/.test(path) || ['chart.yaml', 'values.yaml', 'inventory.yaml'].includes(path)) continue
    }
    try {
      const plays = yaml.load(content) as any[] | null
      if (!Array.isArray(plays)) continue
      for (const item of plays) {
        if (isHandlerFragment) {
          // 片段：顶层就是 handler 列表
          const n = nameOf(item)
          if (n) out.add(String(n))
        } else {
          for (const h of item?.handlers || []) {
            const n = nameOf(h)
            if (n) out.add(String(n))
          }
        }
      }
    } catch { /* 解析失败跳过该文件 */ }
  }
  return [...out].sort()
}
