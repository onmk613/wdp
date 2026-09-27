// taskChartRef 单测：tasks_from/phase 值补全的相位域判定——光标所在任务
// 的 chart 引用优先于全局扫描（多 chart 引用场景回归）。行文法与 ctx.ts
// 的 parseLine 一致，各形态（标量缩写/map 展开/同行 flow）与回退条件覆盖。
import { describe, expect, it } from 'vitest'
import { taskChartRef } from './chartref'

// 光标列取行尾即可：taskChartRef 只用行号定位（值补全时光标在键行/值行上）
function refAt(text: string, line: number) {
  return taskChartRef(text, { lineNumber: line, column: 999 })
}

describe('taskChartRef 标量缩写', () => {
  it('续行 phase: 所在任务块的 chart 值', () => {
    const text = ['- chart: nginx', '  phase: deploy', ''].join('\n')
    expect(refAt(text, 2)).toBe('nginx')
  })
  it('play 形态 tasks 内的任务', () => {
    const text = [
      '- name: p',
      '  tasks:',
      '    - chart: redis',
      '      phase: install',
      '',
    ].join('\n')
    expect(refAt(text, 4)).toBe('redis')
  })
  it('tasks: 与列表项同缩进的仓库写法', () => {
    const text = ['tasks:', '- chart: redis', '  phase: install', ''].join('\n')
    expect(refAt(text, 3)).toBe('redis')
  })
  it('多 chart 引用：取光标所在任务而非全局第一个', () => {
    const text = [
      '- chart: redis',
      '  phase: install',
      '- chart: nginx',
      '  phase: ',
      '',
    ].join('\n')
    expect(refAt(text, 4)).toBe('nginx')
  })
  it('任务里的嵌套块（vars 列表）不阻断向上定位', () => {
    const text = ['- chart: nginx', '  vars:', '    - a', '  phase: ', ''].join('\n')
    expect(refAt(text, 4)).toBe('nginx')
  })
})

describe('taskChartRef map 展开 / flow', () => {
  it('dash 行 chart: 块内 name:', () => {
    const text = ['- chart:', '    name: nginx', '    phase: ', ''].join('\n')
    expect(refAt(text, 3)).toBe('nginx')
  })
  it('续行写法的 chart: 值块内 tasks_from:', () => {
    const text = ['- name: x', '  chart:', '    name: nginx', '    tasks_from: ', ''].join('\n')
    expect(refAt(text, 4)).toBe('nginx')
  })
  it('map 展开带其他子键（values 等）仍取 name', () => {
    const text = [
      '- chart:',
      '    values:',
      '      replicas: 2',
      '    name: nginx',
      '    phase: ',
      '',
    ].join('\n')
    expect(refAt(text, 5)).toBe('nginx')
  })
  it('同行 flow map', () => {
    const text = ['- chart: {name: nginx, phase: ', ''].join('\n')
    expect(refAt(text, 1)).toBe('nginx')
  })
})

describe('taskChartRef 拿不到（调用方回退全局启发式）', () => {
  it('非 chart 任务（shell 等）', () => {
    const text = ['- shell: echo hi', '  phase: ', ''].join('\n')
    expect(refAt(text, 2)).toBeUndefined()
  })
  it('chart 引用名是模板变量', () => {
    const text = ['- chart: "{{ .app.chart }}"', '  phase: ', ''].join('\n')
    expect(refAt(text, 2)).toBeUndefined()
  })
  it('play 级（dash 行键不是 chart）', () => {
    const text = ['- name: web', '  hosts: all', '  phase: ', ''].join('\n')
    expect(refAt(text, 3)).toBeUndefined()
  })
  it('光标在其他控制键的值块内（when: 下的 phase）', () => {
    const text = ['- chart: nginx', '  when:', '    phase: ', ''].join('\n')
    expect(refAt(text, 3)).toBeUndefined()
  })
})
