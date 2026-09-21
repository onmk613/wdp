// lint 消息行号解析与 marker 映射单测。
import { describe, expect, it } from 'vitest'
import { parseIssueLine } from './validate'

describe('parseIssueLine', () => {
  it('yaml.v3 的 "line N:" 形态', () => {
    expect(parseIssueLine('deploy.yaml: yaml: line 7: did not find expected key')).toBe(7)
  })
  it('playbook 的 "file:line:col" 形态', () => {
    expect(parseIssueLine('deploy.yaml:7:3: unknown module "nosuchmod"')).toBe(7)
  })
  it('无行号返回 undefined', () => {
    expect(parseIssueLine('values.yaml: some general problem')).toBeUndefined()
    expect(parseIssueLine('')).toBeUndefined()
  })
})
