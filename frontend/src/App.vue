<script setup lang="ts">
// 根组件：纯路由出口。认证态在 auth 模块（守卫消费），401 事件统一在
// 这里收口——清认证缓存并回登录页（带 redirect 深链回跳）。push 前等
// router.isReady()：初始导航挂起时 push 会打断守卫自己的重定向，形成
// 无限循环（/api/me 的探测因此不走会派发本事件的 api()）。
import { onMounted, onUnmounted } from 'vue'
import { resetAuth } from './auth'
import { router } from './router'

const onUnauthorized = () => {
  resetAuth()
  void router.isReady().then(() => {
    const cur = router.currentRoute.value
    if (cur.path !== '/user/login') {
      void router.push({ path: '/user/login', query: { redirect: cur.fullPath } })
    }
  })
}

onMounted(() => window.addEventListener('wdp-unauthorized', onUnauthorized))
onUnmounted(() => window.removeEventListener('wdp-unauthorized', onUnauthorized))
</script>

<template>
  <router-view />
</template>

<style>
html,
body,
#app {
  margin: 0;
  min-height: 100vh;
}
body {
  background: #f5f7fa;
  font-family:
    -apple-system, 'PingFang SC', 'Microsoft YaHei', 'Helvetica Neue', Arial, sans-serif;
  -webkit-font-smoothing: antialiased;
}
</style>
