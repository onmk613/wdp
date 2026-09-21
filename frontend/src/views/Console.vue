<script setup lang="ts">
import { computed, nextTick, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  Coin, Expand, Fold, Folder, Memo, Monitor, Position, SwitchButton, Tickets, UserFilled,
} from '@element-plus/icons-vue'
import { api } from '../api'
import { authUser, authPerms, can, resetAuth } from '../auth'

const route = useRoute()
const router = useRouter()

// 菜单高亮取自路由：/apps/new 独立高亮；编辑器（/apps/:id/edit）归属「应用列表」；
// 主机详情（/hosts/:id）归属「主机管理」
const activeMenu = computed(() => {
  const p = route.path
  if (p === '/apps/new') return 'apps/new'
  if (p === '/playbook/new') return 'playbook/new'
  const seg = p.split('/')[1] || 'hosts'
  return seg
})
// 侧边栏折叠（窄屏/专注内容时可收起为图标）
const collapsed = ref(false)
const menuRef = ref()
// 折叠会强制收起全部子菜单，展开时按记忆恢复（default-openeds 只在初始化读一次）
const openSubs = ref<string[]>(['apps-group'])

function onSubMenuToggle(index: string, open: boolean) {
  openSubs.value = open
    ? [...new Set([...openSubs.value, index])]
    : openSubs.value.filter((i) => i !== index)
}

function toggleCollapse() {
  collapsed.value = !collapsed.value
  if (!collapsed.value) {
    nextTick(() => openSubs.value.forEach((i) => menuRef.value?.open(i)))
  }
}
// 菜单切换即路由跳转；编辑器是独立路由，天然不会「切走再切回仍停留」
function onMenuSelect(key: string) {
  router.push(`/${key}`)
}

const titleMap: Record<string, string> = {
  hosts: '主机管理',
  'host-detail': '主机详情',
  exec: '远程执行',
  apps: '应用列表',
  'app-new': '新建应用',
  'playbook-new': 'Playbook 编辑器',
  'app-ide': 'Chart 编辑器',
  apprun: '应用执行',
  runs: '执行记录',
  audit: '操作日志',
  pools: '池管理',
  groups: '组管理',
  labels: '标签管理',
  users: '用户与权限',
}
const pageTitle = computed(() => titleMap[String(route.name)] || activeMenu.value)

// 内容区视图 key：IDE 按「模式 + 底本」重建（新建 → 编辑态的 replace、
// 切换基线版本都应重新加载）；其余页面按 path（query 变化不重建）
const viewKey = computed(() => {
  if (route.name === 'app-ide') {
    return route.query.app
      ? `ide-${route.query.app}@${(route.query.base as string) || ''}`
      : `ide-new-${route.query.name || ''}`
  }
  return route.path
})

// 401 事件由 App 收口（清认证态回登录页）；布局层只负责不再拦截
async function logout() {
  try {
    await api('POST', '/api/logout', {})
  } catch {
    /* 本地退出即可 */
  }
  resetAuth()
  void router.push('/user/login')
}
</script>

<template>
  <el-container style="min-height: 100vh">
    <!-- 侧边导航 -->
    <el-aside :width="collapsed ? '64px' : '210px'" class="aside">
      <div class="brand" :class="{ mini: collapsed }">{{ collapsed ? 'w' : 'wdp console' }}</div>
      <el-menu
        ref="menuRef"
        :default-active="activeMenu"
        :default-openeds="['apps-group']"
        :collapse="collapsed"
        :collapse-transition="false"
        @select="onMenuSelect"
        @open="(i: string) => onSubMenuToggle(i, true)"
        @close="(i: string) => onSubMenuToggle(i, false)"
      >
        <el-menu-item index="hosts">
          <el-icon><Monitor /></el-icon><span>主机管理</span>
        </el-menu-item>
        <el-menu-item v-if="can('run:execute')" index="exec">
          <el-icon><Position /></el-icon><span>远程执行</span>
        </el-menu-item>
        <el-menu-item v-if="can('run:view')" index="runs">
          <el-icon><Tickets /></el-icon><span>执行记录</span>
        </el-menu-item>
        <el-menu-item v-if="can('audit:view')" index="audit">
          <el-icon><Memo /></el-icon><span>操作日志</span>
        </el-menu-item>
        <el-sub-menu v-if="can('app:view') || can('app:create') || can('run:execute')" index="apps-group">
          <template #title><el-icon><Coin /></el-icon><span>应用管理</span></template>
          <el-menu-item v-if="can('app:create')" index="apps/new">新建应用</el-menu-item>
          <el-menu-item v-if="can('app:create')" index="playbook/new">Playbook 编辑器</el-menu-item>
          <el-menu-item v-if="can('app:view')" index="apps">应用列表</el-menu-item>
            <el-menu-item v-if="can('run:execute')" index="apprun">应用执行</el-menu-item>
        </el-sub-menu>
        <el-sub-menu v-if="can('registry:manage')" index="scope-group">
          <template #title><el-icon><Folder /></el-icon><span>分类管理</span></template>
          <el-menu-item index="pools">池管理</el-menu-item>
          <el-menu-item index="groups">组管理</el-menu-item>
          <el-menu-item index="labels">标签管理</el-menu-item>
        </el-sub-menu>
        <el-menu-item v-if="can('user:manage')" index="users">
          <el-icon><UserFilled /></el-icon><span>用户与权限</span>
        </el-menu-item>
      </el-menu>
      <div v-if="!collapsed" class="aside-foot">池 · 组 · 标签 · 应用</div>
    </el-aside>

    <el-container>
      <el-header class="topbar" height="56px">
        <div class="top-left">
          <el-icon class="collapse-toggle" :title="collapsed ? '展开菜单' : '收起菜单'" @click="toggleCollapse">
            <Expand v-if="collapsed" />
            <Fold v-else />
          </el-icon>
          <div class="page-title">{{ pageTitle }}</div>
        </div>
        <div class="top-right">
          <el-icon><UserFilled /></el-icon>
          <span>{{ authUser }}</span>
          <el-tag size="small" :type="authPerms.role === 'admin' ? 'danger' : authPerms.role === 'operator' ? 'warning' : 'info'" effect="plain">
            {{ authPerms.role || '-' }}
          </el-tag>
          <el-divider direction="vertical" />
          <el-button link :icon="SwitchButton" @click="logout">退出</el-button>
        </div>
      </el-header>

      <el-main class="main">
        <router-view :key="viewKey" />
      </el-main>
    </el-container>
  </el-container>
</template>

<style scoped>
.aside {
  background: #1f2d3d;
  display: flex;
  flex-direction: column;
}
.aside :deep(.el-menu) {
  background: transparent;
  border-right: none;
}
.aside :deep(.el-menu-item),
.aside :deep(.el-sub-menu__title) {
  color: #cfd8e3;
}
.aside :deep(.el-menu-item.is-active) {
  color: #fff;
  background: #2563eb;
}
.brand {
  color: #fff;
  padding: 18px 16px 14px;
  font-size: 16px;
  font-weight: 600;
  white-space: nowrap;
  overflow: hidden;
}
.brand.mini {
  text-align: center;
  padding: 18px 0 14px;
  font-size: 18px;
}
.aside-foot {
  color: #6b7a8c;
  font-size: 12px;
  padding: 16px;
  margin-top: auto;
}
.topbar {
  background: #fff;
  border-bottom: 1px solid #e4e7ed;
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.top-left {
  display: flex;
  align-items: center;
  gap: 10px;
}
.collapse-toggle {
  font-size: 18px;
  cursor: pointer;
  color: #606266;
}
.collapse-toggle:hover {
  color: #2563eb;
}
.page-title {
  font-size: 15px;
  font-weight: 600;
}
.top-right {
  display: flex;
  align-items: center;
  gap: 6px;
  color: #606266;
}
.main {
  /* 宽屏自适应：不再用固定 max-width 居中（大屏两侧留白过大），
     改全宽 + 合理内边距，表格/卡片随窗口伸展 */
  width: 100%;
  padding: 16px 24px 24px;
  box-sizing: border-box;
}
</style>
