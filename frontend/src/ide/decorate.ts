// 语义着色：在通用 YAML tokenizer 之上，按 playbook 语义给三类键着色
//（模块键加粗蓝 / 控制键橙 / play 键灰绿），并给带编排属性的任务挂行号
// 槽徽标（when/loop/register/notify 一眼可见）。
//
// 用 yaml 包（eemeli）parseDocument 拿带行列的 AST；解析失败保留上一次
// 的装饰（与结构化编辑器"失败不清空"同一道理）。yaml 包的节点类型联合
// 较繁，这里统一按 any 走（只读 range/value/items/type 四个字段）。

import { parseDocument, isMap, isSeq } from 'yaml'
import type { MonacoNs } from './monaco'

export interface DecorateDeps {
  controlKeys: Set<string>
  playKeys: Set<string>
}

interface Mark {
  offset: number
  length: number
  cls: string
}

interface Badge {
  line: number
  cls: string
}

const TASK_FLAG_KEYS: Record<string, string> = {
  when: 'wdp-badge-when',
  loop: 'wdp-badge-loop',
  until: 'wdp-badge-when',
  register: 'wdp-badge-reg',
  notify: 'wdp-badge-reg',
}

function keyText(keyNode: any): string {
  return String(keyNode?.value ?? '')
}

function walkTaskMap(map: any, deps: DecorateDeps, marks: Mark[], badges: Badge[], lineOf: (o: number) => number): void {
  for (const pair of map.items || []) {
    const key = keyText(pair.key)
    const range = pair.key?.range
    if (!key || !range) continue
    if (!deps.controlKeys.has(key) && !deps.playKeys.has(key) && key !== 'name') {
      marks.push({ offset: range[0], length: range[1] - range[0], cls: 'wdp-mod-key' })
    } else if (deps.controlKeys.has(key)) {
      marks.push({ offset: range[0], length: range[1] - range[0], cls: 'wdp-ctl-key' })
      if (TASK_FLAG_KEYS[key]) badges.push({ line: lineOf(range[0]), cls: TASK_FLAG_KEYS[key] })
    }
  }
}

function walkNode(node: any, deps: DecorateDeps, marks: Mark[], badges: Badge[], lineOf: (o: number) => number, inTask: boolean): void {
  if (!node) return
  if (isMap(node)) {
    // 裸任务形态：非任务上下文的 map 若含模块键（如根级序列项直接是
    // "- shell: ..."）按任务 map 着色（模块键/控制键 + 徽标）
    const hasModule = (node.items || []).some((pair0: any) => {
      const k = keyText(pair0?.key)
      return !!k && !deps.controlKeys.has(k) && !deps.playKeys.has(k) && k !== 'name'
    })
    if (inTask || hasModule) {
      walkTaskMap(node, deps, marks, badges, lineOf)
    } else {
      for (const pair0 of node.items || []) {
        const pair = pair0 as any
        const key = keyText(pair.key)
        const kr = pair.key?.range
        if (key && kr) marks.push({ offset: kr[0], length: kr[1] - kr[0], cls: 'wdp-play-key' })
        if (key === 'tasks' || key === 'handlers' || key === 'block' || key === 'rescue' || key === 'always') {
          walkNode(pair.value, deps, marks, badges, lineOf, true)
        }
      }
    }
    return
  }
  if (isSeq(node)) {
    for (const item of node.items || []) walkNode(item, deps, marks, badges, lineOf, inTask)
  }
}

// computeDecorations 返回语义装饰（offset 形态）
export function computeDecorations(
  text: string,
  deps: DecorateDeps,
  getPositionAt: (offset: number) => { lineNumber: number; column: number },
): { marks: Mark[]; badges: Badge[] } | null {
  let doc
  try {
    doc = parseDocument(text)
  } catch {
    return null
  }
  if (doc.errors?.length) return null
  const marks: Mark[] = []
  const badges: Badge[] = []
  const lineOf = (o: number) => getPositionAt(o).lineNumber
  walkNode(doc.contents, deps, marks, badges, lineOf, false)
  return { marks, badges }
}

// applyDecorations 对单个 model 重算并应用装饰（返回新装饰 id 列表；
// 解析失败返回 oldIds 保留上一次）
export function applyDecorations(
  monaco: MonacoNs,
  model: import('monaco-editor').editor.ITextModel,
  oldIds: string[],
  deps: DecorateDeps,
): string[] {
  const computed = computeDecorations(model.getValue(), deps, (o) => model.getPositionAt(o))
  if (!computed) return oldIds
  const newDecos = [
    ...computed.marks.map((m) => {
      const start = model.getPositionAt(m.offset)
      const end = model.getPositionAt(m.offset + m.length)
      return {
        range: new monaco.Range(start.lineNumber, start.column, end.lineNumber, end.column),
        options: { inlineClassName: m.cls },
      }
    }),
    ...computed.badges.map((b) => ({
      range: new monaco.Range(b.line, 1, b.line, 1),
      options: {
        glyphMarginClassName: b.cls,
        glyphMarginHoverMessage: { value: '该任务带编排属性（when/loop/register/notify）' },
      },
    })),
  ]
  return model.deltaDecorations(oldIds, newDecos as any)
}
