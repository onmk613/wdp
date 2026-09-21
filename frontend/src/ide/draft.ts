// 草稿（暂存）：服务端为主（跨设备可恢复），localStorage 为兜底（断网/
// 浏览器崩溃时最近一次快照）。两侧时间戳取新者。保存/校验体由 fs
// 统一序列化，草稿只是它的快照 + UI 状态。

import { api, type DraftResp } from '../api'
import { authUser } from '../auth'
import type { DraftPayload } from './fs'

export class DraftStore {
  constructor(public key: string) {}

  private localKey(): string {
    // 本地兜底草稿按用户隔离：key 不带用户名时，共享浏览器上前一个
    // 用户留下的本地草稿会被下一个用户当自己的恢复（服务端草稿按用户
    // 隔离，但 load() 比时间戳取新者，本地这份会顶上去）。服务端草稿
    // 始终为主，旧键下的本地快照就此作废，无迁移必要
    return `wdp-draft-${authUser.value || 'anon'}-${this.key}`
  }

  readLocal(): DraftPayload | null {
    try {
      const raw = localStorage.getItem(this.localKey())
      return raw ? (JSON.parse(raw) as DraftPayload) : null
    } catch {
      return null
    }
  }

  // load 取最新草稿（服务端与本地比时间戳）；无草稿返回 null
  async load(): Promise<{ payload: DraftPayload; updated_at: string; source: 'server' | 'local' } | null> {
    const local = this.readLocal()
    let server: DraftResp | null = null
    try {
      server = await api<DraftResp>('GET', `/api/apps/draft?key=${encodeURIComponent(this.key)}`)
    } catch {
      server = null // 404（无草稿）或网络错误：本地兜底
    }
    if (server) {
      let payload: DraftPayload | null = null
      try {
        payload = JSON.parse(server.payload)
      } catch {
        payload = null
      }
      if (payload && (!local || local.saved_at <= server.updated_at)) {
        return { payload, updated_at: server.updated_at, source: 'server' }
      }
    }
    if (local) return { payload: local, updated_at: local.saved_at, source: 'local' }
    return null
  }

  async save(payload: DraftPayload, baseVersion: string): Promise<void> {
    // 本地兜底先落（同步、必成功），服务端随后
    try {
      localStorage.setItem(this.localKey(), JSON.stringify(payload))
    } catch { /* 配额满等：服务端仍可写 */ }
    await api('PUT', `/api/apps/draft?key=${encodeURIComponent(this.key)}`, {
      base_version: baseVersion,
      payload: JSON.stringify(payload),
    })
  }

  async clear(): Promise<void> {
    localStorage.removeItem(this.localKey())
    try {
      await api('DELETE', `/api/apps/draft?key=${encodeURIComponent(this.key)}`)
    } catch { /* 无草稿 404 等：清空是幂等语义 */ }
  }
}
