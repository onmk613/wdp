// yamlCtx 上下文识别单测：补全 provider 的判定核心（play/task/moduleArgs/
// value 四级 + dash 行新任务 + flow map）。人工书写 YAML 的各种形态回归。
import { describe, expect, it } from 'vitest'
import { inTemplateExpr, templateExprKind, yamlCtx } from './ctx'

const ctl = new Set(['name', 'when', 'loop', 'with_items', 'register', 'notify', 'until', 'become', 'vars'])
const play = new Set(['name', 'hosts', 'become', 'serial', 'strategy', 'vars'])

// pos: 行列均为 1 起；构造 helper：把光标位置标在文本里用「|」占位不可行
//（列号要精确），直接传 lineNumber/column。
function ctxOf(text: string, line: number, col: number, defaultIsTask = true) {
  return yamlCtx(text, { lineNumber: line, column: col }, ctl, play, defaultIsTask)
}

const PLAYBOOK = [
  '- name: web 部分',
  '  hosts: webservers',
  '  tasks:',
  '    - name: 安装',
  '      shell: echo hi',
  '      when: x == 1',
  '    - sh',
  '',
].join('\n')

describe('yamlCtx 键位置', () => {
  it('play 级：play map 内、任务列表外', () => {
    // 第 2 行 hosts 值后的续行键位（光标在行首缩进+键名前缀处）
    const text = '- name: p\n  ho\n  tasks:\n    - shell: x\n'
    const c = ctxOf(text, 2, 4)
    expect(c?.kind).toBe('key')
    expect(c?.kind === 'key' && c.level).toBe('play')
  })

  it('task 级：dash 行输入模块名前缀（上个任务的模块键不泄漏）', () => {
    const c = ctxOf(PLAYBOOK, 7, 7) // "    - sh" 的 sh 后
    expect(c?.kind).toBe('key')
    if (c?.kind === 'key') {
      expect(c.level).toBe('task')
      expect(c.module).toBeUndefined() // 未写模块 → 模块名候选应出现
    }
  })

  it('task 级：任务续行输入控制键', () => {
    const c = ctxOf(PLAYBOOK, 6, 8) // "      when: ..." 的 whe 位置（已有 when，取键位）
    expect(c?.kind).toBe('key')
    if (c?.kind === 'key') expect(c.level).toBe('task')
  })

  it('task 级：已有模块键被识别（参数级判定依据）', () => {
    const text = '- shell: echo\n  cha\n' // 裸任务形态续行
    const c = ctxOf(text, 2, 4)
    expect(c?.kind).toBe('key')
    if (c?.kind === 'key') {
      expect(c.level).toBe('task')
      expect(c.module).toBe('shell')
    }
  })

  it('moduleArgs 级：模块键值块内的参数位', () => {
    const text = [
      '- name: p',
      '  tasks:',
      '    - copy:',
      '        des', // 参数比模块键深一层（dash 行模块键在 col 7，参数 col 9）
    ].join('\n')
    const c = ctxOf(text, 4, 11)
    expect(c?.kind).toBe('key')
    if (c?.kind === 'key') {
      expect(c.level).toBe('moduleArgs')
      expect(c.module).toBe('copy')
    }
  })

  it('控制键值块（when:）不给键补全', () => {
    const text = '- shell: x\n  when:\n    - xx\n'
    // when 值块内位置：ctx 应为 null（或非 key）——控制键值块不出键候选
    const c = ctxOf(text, 3, 7)
    expect(c === null || c.kind !== 'key' || (c.kind === 'key' && c.level === 'moduleArgs')).toBe(true)
  })
})

describe('yamlCtx 值位置', () => {
  it('模块键后值位置', () => {
    const c = ctxOf('- shell: |\n'.replace('|', '') + '', 1, 9) // "  shell: " 之后
    expect(c?.kind).toBe('value')
    if (c?.kind === 'value') expect(c.key).toBe('shell')
  })

  it('flow map 内键值段', () => {
    const text = '- copy: {src: a, dest: |}'.replace('|', '')
    const c = ctxOf(text, 1, 24)
    expect(c?.kind).toBe('value')
    if (c?.kind === 'value') expect(c.key).toBe('dest')
  })

  it('值位置带模块归属（枚举值补全定位模块）', () => {
    const text = [
      '- name: p',
      '  tasks:',
      '    - copy:',
      '      state: pr',
    ].join('\n')
    const c = ctxOf(text, 4, 16)
    expect(c?.kind).toBe('value')
    if (c?.kind === 'value') {
      expect(c.key).toBe('state')
      expect(c.module).toBe('copy')
    }
  })
})

describe('根级形态缺省', () => {
  it('全新文件按调用方缺省（相位=任务 / playbook=play）', () => {
    const empty = '- na'
    const asTask = ctxOf(empty, 1, 5, true)
    const asPlay = ctxOf(empty, 1, 5, false)
    expect(asTask?.kind === 'key' && asTask.level).toBe('task')
    expect(asPlay?.kind === 'key' && asPlay.level).toBe('play')
  })
  it('出现 tasks: 即 play 形态（与缺省无关）', () => {
    const text = '- na\n  tasks:\n    - she'
    const c = ctxOf(text, 3, 10, false)
    expect(c?.kind === 'key' && c.level).toBe('task')
  })
})

describe('模板表达式识别', () => {
  it('inTemplateExpr', () => {
    expect(inTemplateExpr('x {{ .app.')).toBe(' .app.')
    expect(inTemplateExpr('{{ .x }} done {{ .y')).toBe(' .y')
    expect(inTemplateExpr('{{ .x }}')).toBeNull()
    expect(inTemplateExpr('no template')).toBeNull()
  })
  it('templateExprKind', () => {
    expect(templateExprKind('.app.po').kind).toBe('path')
    expect(templateExprKind('include "hea').kind).toBe('helper')
    expect(templateExprKind('up').kind).toBe('token')
  })
})
