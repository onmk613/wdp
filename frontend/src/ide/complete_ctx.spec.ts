// 补全上下文判定（yamlCtx）的回归：模块参数块内新键、已有键行首、
// dash 新任务等关键位置必须判到 moduleArgs/<模块>——参数候选完全由
// ctx.module 决定（候选表来自 /api/modules 的 ParamDoc，前端零硬编码）。
import { describe, expect, it } from 'vitest'
import { yamlCtx } from './ctx'

// stop.yaml 原样（用户实际文件形态：根级任务 + loop）
const stopYaml = [
  '- name: 停止docker/containerd',
  '  systemd_unit:',
  '    name: "{{ .item }}"',
  '    state: stopped',
  '    enabled: false',
  '  loop: ',
  '    - containerd.service',
  '    - docker.service',
  '',
].join('\n')

// 控制键集合按 schema 全集（与 /api/schema 派生一致的关键子集）
const CK = new Set([
  'name', 'when', 'loop', 'register', 'notify', 'tags', 'until', 'retries',
  'delay', 'timeout', 'ignore_errors', 'become', 'become_user', 'delegate_to',
  'run_once', 'loop_control', 'changed_when', 'failed_when', 'environment',
  'output', 'no_log', 'hook', 'vars', 'tasks_from', 'args', 'block', 'rescue',
  'always', 'include',
])
const PK = new Set(['hosts', 'vars', 'serial', 'strategy', 'become', 'environment', 'name', 'tasks', 'handlers'])

describe('yamlCtx 模块参数块判定（stop.yaml 形态）', () => {
  it('块内新行（enabled 下方缩进4）判 moduleArgs/systemd_unit', () => {
    const text = stopYaml.replace('  loop: ', '    daemon_reload: true\n  loop: ')
    // 光标在第6行（daemon_reload 行）输入中的 "d"
    const ctx = yamlCtx(text, { lineNumber: 6, column: 6 }, CK, PK, true)
    expect(ctx?.kind).toBe('key')
    if (ctx?.kind === 'key') {
      expect(ctx.level).toBe('moduleArgs')
      expect(ctx.module).toBe('systemd_unit')
    }
  })

  it('块内已有键行首（state 行）判 moduleArgs 且 existing 含 name', () => {
    const ctx = yamlCtx(stopYaml, { lineNumber: 4, column: 5 }, CK, PK, true)
    expect(ctx?.kind).toBe('key')
    if (ctx?.kind === 'key') {
      expect(ctx.level).toBe('moduleArgs')
      expect(ctx.module).toBe('systemd_unit')
      expect(ctx.existing.has('name')).toBe(true)
    }
  })

  it('dash 新任务行判 task 级（给模块名补全）', () => {
    const text = stopYaml + '- d'
    const ctx = yamlCtx(text, { lineNumber: 9, column: 3 }, CK, PK, true)
    expect(ctx?.kind).toBe('key')
    if (ctx?.kind === 'key') expect(ctx.level).toBe('task')
  })

  it('deploy.yaml play 形态：任务内模块块同样判定', () => {
    const text = [
      '- hosts: all',
      '  tasks:',
      '    - name: 部署',
      '      systemd_unit:',
      '        name: docker.service',
      '        s',
      '',
    ].join('\n')
    const ctx = yamlCtx(text, { lineNumber: 6, column: 10 }, CK, PK, false)
    expect(ctx?.kind).toBe('key')
    if (ctx?.kind === 'key') {
      expect(ctx.level).toBe('moduleArgs')
      expect(ctx.module).toBe('systemd_unit')
    }
  })

  it('state 值位置判 value + 归属 systemd_unit（枚举值补全依据）', () => {
    const ctx = yamlCtx(stopYaml, { lineNumber: 4, column: 13 }, CK, PK, true)
    expect(ctx?.kind).toBe('value')
    if (ctx?.kind === 'value') {
      expect(ctx.key).toBe('state')
      expect(ctx.module).toBe('systemd_unit')
    }
  })
})
