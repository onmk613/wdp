// 变量域与值补全数据源单测。
import { describe, expect, it } from 'vitest'
import { buildVarDomain, handlerNames, phaseNames, resolvePathPrefix, subchartNames } from './vars'
import type { SchemaMeta } from '../api'

const meta: SchemaMeta = {
  task: [], play: [], chart: [],
  builtin_vars: ['inventory', 'play_hosts'],
  template_funcs: ['upper', 'default'],
}

function domainOf(files: Record<string, string>) {
  return buildVarDomain(new Map(Object.entries(files)), meta)
}

describe('buildVarDomain', () => {
  it('values.yaml 子树 + 内置变量 + item', () => {
    const d = domainOf({
      'values.yaml': 'app:\n  port: 80\n  name: demo\n',
    })
    expect(d.roots['app']?.children?.['port']).toBeDefined()
    expect(d.roots['inventory']?.detail).toContain('内置')
    expect(d.roots['item']).toBeDefined()
    expect(d.funcs).toEqual(['upper', 'default'])
  })
  it('register / loop_var 收集（不覆盖 values 同名）', () => {
    const d = domainOf({
      'deploy.yaml': '- shell: x\n  register: out\n  loop:\n    - a\n  loop_control:\n    loop_var: item2\n',
    })
    expect(d.roots['out']?.detail).toBe('register')
    expect(d.roots['item2']?.detail).toBe('循环变量')
  })
  it('_helpers.tpl 的 define 名收集', () => {
    const d = domainOf({ '_helpers.tpl': '{{- define "app.name" -}}x{{- end -}}' })
    expect(d.helpers).toContain('app.name')
  })
  it('坏 values.yaml 不炸（跳过）', () => {
    const d = domainOf({ 'values.yaml': '{ broken' })
    expect(d.roots['inventory']).toBeDefined()
  })
})

describe('resolvePathPrefix', () => {
  const d = domainOf({ 'values.yaml': 'app:\n  port: 80\n' })
  it('尾点 = 输入下一级', () => {
    const r = resolvePathPrefix(d, '.app.')!
    expect(r.rest).toBe('')
    expect(Object.keys(r.node.children || {})).toContain('port')
  })
  it('半截段 = 正在输入', () => {
    const r = resolvePathPrefix(d, '.app.po')!
    expect(r.rest).toBe('po')
  })
  it('脱离已知域返回 null', () => {
    expect(resolvePathPrefix(d, '.nope.')).toBeNull()
  })
})

describe('值补全数据源', () => {
  it('subchartNames 从 charts/ 收集', () => {
    expect(subchartNames(['charts/redis/chart.yaml', 'charts/nginx/deploy.yaml', 'deploy.yaml']))
      .toEqual(['nginx', 'redis'])
  })
  it('phaseNames 根相位与子 chart 相位', () => {
    const paths = ['deploy.yaml', 'backup.yaml', 'chart.yaml', 'values.yaml',
      'charts/redis/reload.yaml', 'charts/redis/chart.yaml']
    expect(phaseNames(paths)).toEqual(['backup', 'deploy'])
    expect(phaseNames(paths, 'redis')).toEqual(['reload'])
  })
  it('handlerNames：play 内联 + handlers/ 片段', () => {
    const files = new Map([
      ['deploy.yaml', '- name: p\n  handlers:\n    - name: restart docker\n      service: {name: docker, state: restarted}\n'],
      ['handlers/ping.yaml', '- name: ping\n  shell: ping -c1 localhost\n'],
    ])
    expect(handlerNames(files, new Set(['name', 'when']))).toEqual(['ping', 'restart docker'])
  })
})
