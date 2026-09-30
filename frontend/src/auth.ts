// 认证状态（模块级单例）：路由守卫、Console 头部、登录页共享。
// /api/me 只探一次（Promise 缓存）；登录/退出/401 事件重置缓存。
// 探测用裸 fetch 而非 api()：401 不能派发 wdp-unauthorized——那会与
// 守卫的重定向导航竞争（初始导航挂起时事件处理器 push 登录页，打断
// 守卫自己的跳转，形成无限重定向）。
// 权限摘要（role/verbs/scoped）随 me 一起返回：can() 是前端门控（菜单/
// 按钮可见性），后端强制才是安全边界。
import { computed, ref } from 'vue'

export interface PermScope { kind: string; value: string }
export interface Perms {
  role: string
  verbs: string[] | null
  global: string[] | null
  scoped: Record<string, PermScope[]> | null
}

export const authUser = ref('')
export const authPerms = ref<Perms>({ role: '', verbs: null, global: null, scoped: null })

const verbSet = computed(() => new Set(authPerms.value.verbs || []))

// can 前端权限判定（有该权限点即可见对应入口；作用域裁剪由后端做）
export function can(verb: string): boolean {
  return verbSet.value.has(verb)
}

// server 端构建版本（远程升级目标版本）：me 响应带回，升级按钮门控用。
// serverBuildUnversioned：裸 go build 的开发构建（未注入构建信息），版本串
// 不反映二进制内容——同串不能证明同版本，升级门控按可升级放行
export const serverBuild = ref('')
export const serverBuildUnversioned = ref(false)

let mePromise: Promise<{ user: string; role: string; perms: Perms; build?: string } | null> | null = null

function fetchMe() {
  if (mePromise) return mePromise
  const p = fetch('/api/me', { headers: { Accept: 'application/json' } }).then(async (r) => {
    if (r.ok) {
      const me = (await r.json()) as {
        user: string; role: string; perms: Perms; build?: string; build_unversioned?: boolean
      }
      if (me.build) serverBuild.value = me.build
      serverBuildUnversioned.value = !!me.build_unversioned
      return me
    }
    // 仅明确的未登录（401/403）算「未登录」返回 null；其余状态码与网络层
    // reject（瞬时故障）一律抛错——否则网络抖动会被守卫当未登录跳登录页
    if (r.status === 401 || r.status === 403) return null
    throw new Error(`探测登录态失败（HTTP ${r.status}）`)
  })
  // 失败结果不缓存：下次导航重新探测（成功结果仍缓存整个会话）
  p.catch(() => {
    if (mePromise === p) mePromise = null
  })
  mePromise = p
  return p
}

// ensureAuthed 守卫用：确保认证态已就绪，返回当前用户（空 = 未登录）。
// 网络错误向上抛出，由守卫决定放行（页面内 API 调用会自行报错）
export async function ensureAuthed(): Promise<string> {
  const me = await fetchMe()
  if (me) {
    authUser.value = me.user
    // role 在响应顶层、perms 摘要里没有——合并，两条路径（登录 setAuth
    // 与刷新后 me 重探）口径一致
    authPerms.value = { ...(me.perms || { verbs: null, global: null, scoped: null }), role: me.role }
  }
  return me ? me.user : ''
}

export function setAuth(user: string, perms?: Perms, build?: string, buildUnversioned?: boolean) {
  authUser.value = user
  if (perms) authPerms.value = perms
  if (build) serverBuild.value = build
  if (buildUnversioned !== undefined) serverBuildUnversioned.value = buildUnversioned
  // 守卫下次导航读缓存：写入已解析的用户（含权限摘要），避免重探
  mePromise = Promise.resolve({ user, role: perms?.role || '', perms: perms || authPerms.value, build: serverBuild.value || undefined })
}

// resetAuth 登出 / 401 时清态；下次守卫会重新探 /api/me。
// 本机草稿一并清除：chart/playbook 全文可能含口令、内网地址等敏感内容，
// 草稿键虽按用户隔离恢复路径，但共享浏览器上数据本身长期残留可读——
// 会话结束（登出/401）即失效，残留无意义且属泄露面
export function resetAuth() {
  clearLocalDrafts(authUser.value)
  authUser.value = ''
  mePromise = null
  authPerms.value = { role: '', verbs: null, global: null, scoped: null }
}

// clearLocalDrafts 清掉该用户的本机草稿（ide/draft.ts 的 `wdp-draft-<user>-*`
// 与 PlaybookPage 的 `wdp-playbook-draft-<user>`，键形态见各自实现）。
function clearLocalDrafts(user: string) {
  try {
    const chartPrefix = `wdp-draft-${user || 'anon'}-`
    const playbookKey = `wdp-playbook-draft-${user || 'anon'}`
    const hits: string[] = []
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i)
      if (k === playbookKey || (k && k.startsWith(chartPrefix))) hits.push(k)
    }
    for (const k of hits) localStorage.removeItem(k)
  } catch {
    /* localStorage 不可用（隐私模式等）：无可清理 */
  }
}
