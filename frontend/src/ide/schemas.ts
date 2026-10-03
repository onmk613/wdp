// JSON Schema 装配：把 /api/schema 的字段文档表（后端单一事实来源）转成
// monaco-yaml 的 per-file schema——控制键/play 键/chart.yaml 键的补全、
// 类型校验、hover 文档全部由此驱动。

import type { FieldSection, SchemaMeta } from '../api'
import { api } from '../api'
import { configureYaml } from './monaco'

export async function fetchSchema(): Promise<SchemaMeta> {
  return api<SchemaMeta>('GET', '/api/schema')
}

// 控制键清单（模块键判定：任务 map 里的非控制键即模块名）。meta 未加载
// 时返回空集合（视图侧 null 守卫统一收在这里）
export function controlKeys(meta: SchemaMeta | null): Set<string> {
  const s = new Set<string>()
  if (!meta) return s
  for (const sec of meta.task) {
    for (const f of sec.fields) s.add(f.name)
  }
  // chart 引用/模块键写法不算控制键（它们写在模块位置）
  for (const k of ['chart', 'include', 'values', 'values_from', 'hosts', 'phase', 'args']) s.delete(k)
  return s
}

function descOf(f: FieldSection['fields'][number]): string {
  return `${f.desc}${f.default && f.default !== '-' ? `（默认 ${f.default}）` : ''}`
}

function jsonType(t: string, taskRef?: string): any {
  if (t === 'bool') return { type: 'boolean' }
  if (t === 'int') return { type: 'integer' }
  if (t === 'string') return { type: 'string' }
  if (t === 'map') return { type: 'object' }
  if (t === 'list<task>' && taskRef) return { type: 'array', items: { $ref: taskRef } }
  return {} // 联合类型（string | list 等）不约束，避免误报
}

function propsFrom(sections: FieldSection[], taskRef?: string): Record<string, any> {
  const out: Record<string, any> = {}
  for (const sec of sections) {
    for (const f of sec.fields) {
      if (taskRef && ['block', 'rescue', 'always'].includes(f.name)) {
        out[f.name] = { type: 'array', items: { $ref: taskRef }, description: descOf(f) }
        continue
      }
      out[f.name] = { ...jsonType(f.type, taskRef), description: descOf(f) }
    }
  }
  return out
}

// playbookSchema：deploy.yaml / <相位>.yaml / tasks 片段
export function playbookSchema(meta: SchemaMeta): object {
  const taskRef = '#/definitions/task'
  const taskProps = propsFrom(meta.task, taskRef)
  return {
    type: 'array',
    // 相位文件两种形态：play（编排）或裸任务（简单相位直接写任务）
    items: { anyOf: [{ $ref: '#/definitions/play' }, { $ref: taskRef }] },
    definitions: {
      play: {
        type: 'object',
        properties: {
          ...propsFrom(meta.play),
          tasks: { type: 'array', items: { $ref: taskRef }, description: '主任务列表（按序执行）' },
          handlers: { type: 'array', items: { $ref: taskRef }, description: '处理器：notify 触发，play 末尾统一 flush' },
        },
        additionalProperties: true,
      },
      task: {
        type: 'object',
        properties: taskProps,
        // 模块键（shell/copy/…）任意出现，schema 不拦（补全与校验由 provider/lint 负责）
        additionalProperties: true,
      },
    },
  }
}

// chartYAMLSchema：chart.yaml 全字段
export function chartYAMLSchema(meta: SchemaMeta): object {
  const phaseProps: Record<string, any> = {
    release: { type: 'boolean', description: '部署语义（required 校验/可逆性/marker）' },
    record: { type: 'boolean', description: '记部署记录' },
    clears_marker: { type: 'boolean', description: '成功后清 marker' },
    values_from: { type: 'string', description: '空 | chart | marker' },
  }
  return {
    type: 'object',
    properties: {
      name: { type: 'string', description: 'chart 名（与库名对账，必须一致）' },
      version: { type: 'string', description: '版本（与保存的版本号对账，必须一致）' },
      description: { type: 'string', description: '说明文本' },
      ...propsFrom(meta.chart),
      phases: {
        type: 'object',
        description: '相位属性声明：键 = 相位名（须有对应 <相位名>.yaml 文件）',
        additionalProperties: { type: 'object', properties: phaseProps, additionalProperties: false },
      },
    },
    additionalProperties: true,
  }
}

// applyYamlSchemas 按当前文件清单装配 schema：根目录相位 yaml（非保留名）
// 与 tasks/ 片段挂 playbook schema，chart.yaml 挂元数据 schema。文件增删
// /改名后重新调用（monaco-yaml 可重复配置）。
export function applyYamlSchemas(paths: string[], meta: SchemaMeta, valuesSchemaJSON?: string): void {
  const rootPlaybooks = ['deploy.yaml']
  const reserved = new Set(['chart.yaml', 'values.yaml', 'inventory.yaml'])
  for (const p of paths) {
    const m = p.match(/^([A-Za-z0-9_-]+)\.yaml$/)
    if (m && !reserved.has(p) && p !== 'deploy.yaml') rootPlaybooks.push(p)
  }
  const schemas: any[] = [
    {
      uri: 'wdp-schema://playbook',
      fileMatch: [...rootPlaybooks.map((p) => `wdpchart:/${p}`), 'wdpchart:/tasks/**'],
      schema: playbookSchema(meta),
    },
    {
      uri: 'wdp-schema://chart',
      fileMatch: ['wdpchart:/chart.yaml'],
      schema: chartYAMLSchema(meta),
    },
  ]
  // chart 自带 values.schema.json：values.yaml 按它校验（编辑即生效）
  if (valuesSchemaJSON) {
    try {
      const parsed = JSON.parse(valuesSchemaJSON)
      schemas.push({
        uri: 'wdp-schema://values',
        fileMatch: ['wdpchart:/values.yaml'],
        schema: parsed,
      })
    } catch { /* 损坏的 schema 文件：lint 会报，这里静默跳过 */ }
  }
  configureYaml({
    enableSchemaRequest: false,
    validate: true,
    completion: false, // 键补全由 complete.ts 独占（避免双 provider 同名重复）
    hover: true,
    format: false,
    schemas,
  })
}
