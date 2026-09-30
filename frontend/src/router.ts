// history 模式路由：每个页面有真实 URL，可刷新/直链/收藏。
// 后端 static.go 对非 /api、/enroll 的未匹配 GET 回退 index.html（SPA
// fallback），dev 下 vite 默认自带 history 回退，两端行为一致。
// 认证：全局前置守卫探 /api/me（Promise 缓存，一次会话只探一次）；
// 未登录访问任意页 → /user/login（带 redirect 深链回跳），已登录访问
// 登录页 → 回控制台。
// 授权：受限路由带 meta.perm（与后端权限点字符串一致）；守卫只验「有该
// verb」（作用域裁剪由 API 按资源把关），不满足 → 跳 /hosts 并提示。
import { createRouter, createWebHistory } from 'vue-router'

export { safeRedirect }
import { ElMessage } from 'element-plus'
import { can, ensureAuthed } from './auth'
import { safeRedirect } from './redirect'

// 视图全部按需加载：此前全部静态 import，首屏会把所有页面（含 Chart IDE
// 的 monaco 依赖图）一起拉下来；登录页也要付这份代价。
// 登录页与外壳保留静态（首屏必然用到），其余路由在导航到时才取。
import Login from './views/Login.vue'
import Console from './views/Console.vue'

const HostsPage = () => import('./views/HostsPage.vue')
const HostDetail = () => import('./views/HostDetail.vue')
const ExecPage = () => import('./views/ExecPage.vue')
const AppsPage = () => import('./views/AppsPage.vue')
const AppIDE = () => import('./views/AppIDE.vue')
const AppNew = () => import('./views/AppNew.vue')
const AppRunPage = () => import('./views/AppRunPage.vue')
const PlaybookPage = () => import('./views/PlaybookPage.vue')
const RunsPage = () => import('./views/RunsPage.vue')
const AuditPage = () => import('./views/AuditPage.vue')
const ScopePage = () => import('./views/ScopePage.vue')
const UsersPage = () => import('./views/UsersPage.vue')

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    // 登录页（独立全屏，无控制台布局）
    { path: '/user/login', name: 'login', component: Login },
    {
      path: '/',
      component: Console, // 侧边栏布局，子页共用
      children: [
        { path: '', redirect: '/hosts' },
        { path: 'hosts', name: 'hosts', component: HostsPage, meta: { perm: 'host:view' } },
        { path: 'hosts/:id(\\d+)', name: 'host-detail', component: HostDetail, meta: { perm: 'host:view' } },
        { path: 'exec', name: 'exec', component: ExecPage, meta: { perm: 'run:execute' } },
        { path: 'runs', name: 'runs', component: RunsPage, meta: { perm: 'run:view' } },
        { path: 'audit', name: 'audit', component: AuditPage, meta: { perm: 'audit:view' } },
        { path: 'apps', name: 'apps', component: AppsPage, meta: { perm: 'app:view' } },
        { path: 'apps/new', name: 'app-new', component: AppNew, meta: { perm: 'app:create' } },
        // Chart IDE：query 区分模式——?new=1&name=…（新建脚手架）或
        // ?app=<id>&base=<version>（编辑底本）。单一路由记录：新建保存成功
        // 后 replace 成编辑态不换组件
        {
          path: 'apps/ide',
          name: 'app-ide',
          component: AppIDE,
          props: (route) => ({
            appId: Number(route.query.app) || 0,
            baseVersion: (route.query.base as string) || '',
            newName: (route.query.name as string) || '',
            newVersion: (route.query.version as string) || '',
            newDesc: (route.query.desc as string) || '',
            // 草稿箱「继续编辑」直达：恢复草稿不再走确认横幅
            restore: route.query.restore === '1',
          }),
          meta: { perm: 'app:view' },
        },
        { path: 'apprun', name: 'apprun', component: AppRunPage, meta: { perm: 'app:view' } },
        // 裸 playbook 编辑器（下载后 CLI 执行；不进应用库）
        { path: 'playbook/new', name: 'playbook-new', component: PlaybookPage, meta: { perm: 'app:create' } },
        // 池 / 组 / 标签管理（同一组件三种实例）
        { path: 'pools', name: 'pools', component: ScopePage, props: { kind: 'pool' }, meta: { perm: 'registry:manage' } },
        { path: 'groups', name: 'groups', component: ScopePage, props: { kind: 'group' }, meta: { perm: 'registry:manage' } },
        { path: 'labels', name: 'labels', component: ScopePage, props: { kind: 'label' }, meta: { perm: 'registry:manage' } },
        { path: 'users', name: 'users', component: UsersPage, meta: { perm: 'user:manage' } },
        // 系统设置：admin 专属（后端 requireAdmin 双保险；user:manage 是
        // 内置角色里唯一 admin-only 的 verb，借既有守卫机制收敛入口）
        { path: 'settings', name: 'settings', component: () => import('./views/SettingsPage.vue'), meta: { perm: 'user:manage' } },
      ],
    },
    // 兜底：拼错的地址回主机列表，不给空白页
    { path: '/:pathMatch(.*)*', redirect: '/hosts' },
  ],
})

router.beforeEach(async (to) => {
  let user = ''
  try {
    user = await ensureAuthed()
  } catch {
    // 网络抖动（非 401）：放行导航，页面内 API 调用会自行报错；探测失败
    // 不缓存，下次导航重试。不能在此跳登录页——那等于把瞬时故障当未登录
    return true
  }
  if (to.path === '/user/login') {
    // 已登录再访问登录页 → 回控制台（honor 深链 redirect）
    if (user) return safeRedirect(to.query.redirect)
    return true
  }
  if (!user) {
    return { path: '/user/login', query: to.fullPath === '/' ? {} : { redirect: to.fullPath } }
  }
  // 授权：受限路由验权限点（verb 级；资源级作用域由后端 API 把关）
  const perm = to.meta.perm as string | undefined
  if (perm && !can(perm)) {
    if (to.path !== '/hosts') {
      ElMessage.warning(`无 ${perm} 权限，已跳回主机列表`)
      return '/hosts'
    }
    // 兜底：连 /hosts 都无权限（极端裁剪账号）不再重定向，避免循环；页面内报错
    return true
  }
  return true
})
