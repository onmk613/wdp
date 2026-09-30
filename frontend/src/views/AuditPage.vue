<script setup lang="ts">
// 操作日志：控制台增删改动作的审计视图（谁在何时对什么做了什么）。
// 执行类动作不在此表——执行记录页已带触发者。关键字对用户/动作/对象/
// 名称/详情模糊过滤。
import { onMounted, onUnmounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Refresh, Search } from '@element-plus/icons-vue'
import { api, type AuditLog } from '../api'
import { fmtTime } from '../lib/format'
import type { PagedResp } from '../lib/usePaging'
import Pager from '../components/Pager.vue'

const logs = ref<AuditLog[]>([])
const loading = ref(false)
const q = ref('')
let searchTimer: number | undefined

const page = ref(1)
const pageSize = ref(50)
const total = ref(0)

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    const params = new URLSearchParams({ page: String(page.value), page_size: String(pageSize.value) })
    if (q.value.trim()) params.set('q', q.value.trim())
    const r = await api<PagedResp<AuditLog>>('GET', `/api/audit?${params}`)
    logs.value = r.items
    total.value = r.total
    if (r.items.length === 0 && r.total > 0 && page.value > 1) {
      page.value = Math.ceil(r.total / pageSize.value)
      return load(silent)
    }
  } catch (e) {
    if (!silent) ElMessage.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

function onSearch() {
  window.clearTimeout(searchTimer)
  searchTimer = window.setTimeout(() => { page.value = 1; load() }, 300)
}

// 动作 → 标签颜色（增=绿、改=橙、删=红、登录/其他=灰蓝）
function actionType(a: string): 'success' | 'warning' | 'danger' | 'info' {
  if (a === 'create' || a === 'upload' || a === 'install' || a === 'import') return 'success'
  if (a === 'update' || a === 'set_latest' || a.startsWith('batch_assign')) return 'warning'
  if (a.includes('delete')) return 'danger'
  return 'info'
}

function actionLabel(a: string): string {
  const map: Record<string, string> = {
    create: '新增', update: '修改', delete: '删除', import: '导入',
    upload: '上传', install: '安装', login: '登录', logout: '登出',
    login_failed: '登录失败', set_latest: '设默认',
    batch_probe: '批量探活', batch_delete: '批量删除', batch_assign: '批量设置',
  }
  return map[a] || a
}

onMounted(() => load())
// 防抖定时器随组件销毁清理，避免离开后仍触发一次已卸载页面的 load
onUnmounted(() => window.clearTimeout(searchTimer))
</script>

<template>
  <div>
    <div class="toolbar">
      <el-input
        v-model="q" :prefix-icon="Search" clearable style="width: 260px"
        placeholder="搜索：用户 / 动作 / 对象 / 名称 / 详情"
        @input="onSearch" @clear="load()"
      />
      <span class="muted">记录控制台的新增 / 修改 / 删除 / 登录等动作；执行类见「执行记录」</span>
      <div style="flex: 1" />
      <el-button :icon="Refresh" @click="load()">刷新</el-button>
    </div>

    <el-card shadow="never">
      <el-table :data="logs" v-loading="loading" style="width: 100%">
        <el-table-column prop="CreatedAt" label="时间" width="170">
          <template #default="{ row }"><span class="muted">{{ fmtTime(row.CreatedAt) }}</span></template>
        </el-table-column>
        <el-table-column label="用户" width="100">
          <template #default="{ row }">{{ row.User || '-' }}</template>
        </el-table-column>
        <el-table-column label="动作" width="110">
          <template #default="{ row }">
            <el-tooltip :content="row.Action" :disabled="actionLabel(row.Action) === row.Action" placement="top">
              <el-tag :type="actionType(row.Action)" size="small" effect="light" round>{{ actionLabel(row.Action) }}</el-tag>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="对象" width="110">
          <template #default="{ row }">
            <el-tag size="small" effect="plain">{{ row.Object }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="名称" min-width="150" show-overflow-tooltip>
          <template #default="{ row }">{{ row.Name || '-' }}</template>
        </el-table-column>
        <el-table-column label="详情" min-width="260">
          <template #default="{ row }">
            <el-tooltip :content="row.Detail || '-'" :disabled="!row.Detail" placement="top" :show-after="300">
              <span class="muted detail-text">{{ row.Detail || '-' }}</span>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="来源 IP" width="130">
          <template #default="{ row }"><span class="muted">{{ row.IP || '-' }}</span></template>
        </el-table-column>
        <template #empty><el-empty description="暂无操作记录" /></template>
      </el-table>
      <Pager :page="page" :page-size="pageSize" :total="total" @update:page="(v) => { page = v; load(true) }" @update:page-size="(v) => { pageSize = v; load(true) }" />
    </el-card>
  </div>
</template>

<style scoped>
.toolbar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 12px;
}
.muted {
  color: #909399;
  font-size: 12px;
}
.detail-text {
  display: inline-block;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  vertical-align: bottom;
}
</style>
