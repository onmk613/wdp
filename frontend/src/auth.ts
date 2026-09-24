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

let mePromise: Promise<{ user: string; role: string; perms: Perms } | null> | null = null

function fetchMe() {
  if (mePromise) return mePromise
  const p = fetch('/api/me', { headers: { Accept: 'application/json' } }).then(async (r) => {
    if (r.ok) return (await r.json()) as { user: string; role: string; perms: Perms }
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

export function setAuth(user: string, perms?: Perms) {
  authUser.value = user
  if (perms) authPerms.value = perms
  // 守卫下次导航读缓存：写入已解析的用户（含权限摘要），避免重探
  mePromise = Promise.resolve({ user, role: perms?.role || '', perms: perms || authPerms.value })
}

// resetAuth 登出 / 401 时清态；下次守卫会重新探 /api/me
export function resetAuth() {
  authUser.value = ''
  mePromise = null
  authPerms.value = { role: '', verbs: null, global: null, scoped: null }
}
