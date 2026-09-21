// 整体校验：POST /api/apps/validate（后端 chart.Load 结构自检 + chart.Lint
// 全量静态校验）→ 问题面板 + Monaco markers。保存门禁：存在 ERROR 阻断
//（用户已确认该决策），WARN 仅提示。
//
// 行号：后端 LintIssue 自带结构化 line（任务级发现）；消息内行号解析仅作
// 兜底（yaml.v3 语法错等仍是自由文本消息）。

import { api, type SpecSaveBody, type ValidateIssue } from '../api'
import type { MonacoNs } from './monaco'

export interface ProblemItem extends ValidateIssue {
  line?: number
}

// parseIssueLine 从 lint 消息里解析行号（yaml.v3 错误含 "line N:"；
// playbook 解析错误含 "deploy.yaml:7:3" 形态）
export function parseIssueLine(msg: string): number | undefined {
  let m = msg.match(/line (\d+)/)
  if (m) return parseInt(m[1], 10)
  m = msg.match(/:(\d+):\d+/)
  if (m) return parseInt(m[1], 10)
  return undefined
}

export async function runValidate(body: SpecSaveBody, appId: number): Promise<ProblemItem[]> {
  const resp = await api<{ issues: ValidateIssue[] }>('POST', '/api/apps/validate', {
    ...body,
    app_id: appId || undefined,
  })
  return (resp.issues || []).map((is) => ({
    ...is,
    line: is.line || parseIssueLine(is.msg),
  }))
}

// applyMarkers 把问题映射到各文件的 model（marker owner 'wdp-lint'，
// 与 monaco-yaml 的 'yaml' 诊断并存）
export function applyMarkers(
  monaco: MonacoNs,
  models: Map<string, import('monaco-editor').editor.ITextModel>,
  problems: ProblemItem[],
): void {
  const byPath = new Map<string, ProblemItem[]>()
  for (const p of problems) {
    if (!p.path) continue
    const arr = byPath.get(p.path) || []
    arr.push(p)
    byPath.set(p.path, arr)
  }
  for (const [path, model] of models) {
    const list = byPath.get(path) || []
    monaco.editor.setModelMarkers(
      model,
      'wdp-lint',
      list.map((p) => ({
        startLineNumber: p.line || 1,
        startColumn: 1,
        endLineNumber: p.line || 1,
        endColumn: 2,
        message: p.msg,
        severity:
          p.level === 'ERROR'
            ? monaco.MarkerSeverity.Error
            : monaco.MarkerSeverity.Warning,
        source: 'wdp-lint',
      })),
    )
  }
}
