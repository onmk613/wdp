<script setup lang="ts">
// 登录页（独立路由 /user/login）：成功后 setAuth 并跳 redirect 深链目标
// （守卫塞进来的原地址），无目标回主机列表。
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { Lock, User } from '@element-plus/icons-vue'
import { api } from '../api'
import { setAuth, type Perms } from '../auth'
import { safeRedirect } from '../router'

const route = useRoute()
const router = useRouter()

const form = ref({ user: '', password: '' })
const loading = ref(false)

async function submit() {
  if (loading.value) return // 密码框回车不受按钮 loading 禁用约束，慢网络下连按会并发登录
  if (!form.value.user || !form.value.password) {
    ElMessage.warning('请输入用户名与密码')
    return
  }
  loading.value = true
  try {
    const r = await api<{
      user: string; role: string; perms: Perms; build?: string; build_unversioned?: boolean
    }>('POST', '/api/login', form.value)
    setAuth(r.user, { ...r.perms, role: r.role }, r.build, r.build_unversioned)
    void router.push(safeRedirect(route.query.redirect))
  } catch (e) {
    // 登录端点的 401 已在 api() 豁免全局处理，这里拿到的是服务器原始
    // 错误（invalid credentials / too many failed attempts …），映射成用户
    // 能看懂的中文，其余原样透出
    const msg = (e as Error).message
    if (msg.includes('invalid credentials')) ElMessage.error('用户名或密码错误')
    else if (msg.includes('too many failed attempts')) ElMessage.error('失败次数过多，请稍后再试')
    else ElMessage.error(msg)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="login-wrap">
    <el-card class="login-card">
      <template #header>
        <div class="login-title">wdp console</div>
        <div class="login-sub">主机纳管 · 探活 · 部署</div>
      </template>
      <el-form @submit.prevent="submit">
        <el-form-item>
          <el-input v-model="form.user" placeholder="用户名" autocomplete="username" size="large">
            <template #prefix><el-icon><User /></el-icon></template>
          </el-input>
        </el-form-item>
        <el-form-item>
          <el-input
            v-model="form.password"
            type="password"
            placeholder="密码"
            autocomplete="current-password"
            size="large"
            show-password
            @keyup.enter="submit"
          >
            <template #prefix><el-icon><Lock /></el-icon></template>
          </el-input>
        </el-form-item>
        <el-button type="primary" size="large" style="width: 100%" :loading="loading" @click="submit">
          登 录
        </el-button>
      </el-form>
    </el-card>
  </div>
</template>


<style scoped>
.login-wrap {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background: linear-gradient(160deg, #1f2d3d 0%, #2b4a6f 60%, #2563eb 100%);
}
.login-card {
  width: 360px;
}
.login-title {
  font-size: 20px;
  font-weight: 600;
}
.login-sub {
  font-size: 12px;
  color: #909399;
  margin-top: 4px;
}
</style>
