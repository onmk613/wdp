import { describe, expect, it } from 'vitest'
import { safeRedirect } from './redirect'

describe('safeRedirect 开放重定向防御', () => {
  it('站内路径放行', () => {
    expect(safeRedirect('/hosts')).toBe('/hosts')
    expect(safeRedirect('/apps/ide?app=3')).toBe('/apps/ide?app=3')
  })
  it('协议相对形态拒绝（//evil.com 跨源）', () => {
    expect(safeRedirect('//evil.com')).toBe('/hosts')
    expect(safeRedirect('///evil.com')).toBe('/hosts')
  })
  it('绝对 URL / 外部地址 / 非字符串拒绝', () => {
    expect(safeRedirect('https://evil.com')).toBe('/hosts')
    expect(safeRedirect('evil.com')).toBe('/hosts')
    expect(safeRedirect('')).toBe('/hosts')
    expect(safeRedirect(undefined)).toBe('/hosts')
    expect(safeRedirect(123)).toBe('/hosts')
  })
})
