<script setup lang="ts">
// 执行记录：应用执行（kind=app，多应用按序各一行 run）与远程命令（kind=exec）
// 的统一审计视图。列表 5s 自动刷新，点击行查看任务明细与逐任务输出。
import { onMounted, onUnmounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { can } from '../auth'
import { Delete, Refresh } from '@element-plus/icons-vue'
import { api, subscribeRuns, type Run, type RunTask } from '../api'

const runs = ref<Run[]>([])
const loading = ref(false)
const kindFilter = ref('')

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    runs.value = await api<Run[]>('GET', '/api/runs?limit=100')
  } catch (e) {
    if (!silent) ElMessage.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

// ---- 勾选与删除 ----
const selection = ref<Run[]>([])
const tableRef = ref()

function onSelectionChange(rows: Run[]) {
  selection.value = rows
}

function clearSelection() {
  tableRef.value?.clearSelection()
}

async function removeRun(r: Run) {
  try {
    await ElMessageBox.confirm(`删除执行记录 run #${r.ID}（${r.AppName || '远程命令'}）？任务明细一并删除，不可恢复。`, '删除记录', {
      type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    await api('DELETE', `/api/runs/${r.ID}`)
    ElMessage.success('已删除')
    if (detailRun.value?.ID === r.ID) detailVisible.value = false
    load()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

async function batchDelete() {
  const n = selection.value.length
  try {
    await ElMessageBox.confirm(`删除选中的 ${n} 条执行记录？任务明细一并删除，不可恢复。`, '批量删除记录', {
      type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    const r = await api<{ ok: number; failed: number }>('POST', '/api/runs/batch', {
      ids: selection.value.map((x) => x.ID),
    })
    if (r.failed === 0) ElMessage.success(`已删除 ${r.ok} 条记录`)
    else ElMessage.warning(`删除完成：${r.ok} 成功 / ${r.failed} 失败`)
    clearSelection()
    load()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

const detailVisible = ref(false)
const detailRun = ref<Run | null>(null)
const detailTasks = ref<RunTask[]>([])
const detailLoading = ref(false)

async function openRun(r: Run) {
  detailVisible.value = true
  detailLoading.value = true
  detailRun.value = r
  detailTasks.value = []
  try {
    const d = await api<{ run: Run; tasks: RunTask[] }>('GET', `/api/runs/${r.ID}`)
    // 竞态守卫：等待期间用户可能已点开另一条 run（慢响应后到会整体
    // 替换抽屉内容，且 refreshDetail 之后一直刷错的那条）
    if (detailRun.value?.ID !== r.ID) return
    detailRun.value = d.run
    detailTasks.value = d.tasks
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    detailLoading.value = false
  }
}

// 详情打开时跟随刷新（非终态 run 实时更新：running 与 queued 都会推进）
const RUN_TERMINAL = new Set(['succeeded', 'failed'])
async function refreshDetail() {
  if (!detailVisible.value || !detailRun.value) return
  if (RUN_TERMINAL.has(detailRun.value.Status)) return
  try {
    const d = await api<{ run: Run; tasks: RunTask[] }>('GET', `/api/runs/${detailRun.value.ID}`)
    detailRun.value = d.run
    detailTasks.value = d.tasks
  } catch {
    /* 静默 */
  }
}

function runStatus(s: string): 'success' | 'danger' | 'info' | 'warning' {
  return s === 'succeeded' ? 'success' : s === 'failed' ? 'danger' : s === 'queued' ? 'warning' : 'info'
}

function taskStatus(s: string): 'success' | 'info' | 'danger' {
  return s === 'ok' ? 'success' : s === 'skipped' ? 'info' : 'danger'
}

// 执行事件流（SSE）：任何 run 状态/任务落库都推一条 → 即时刷新。
// 轮询降为兜底：收到过事件说明 SSE 通了，间隔从 5s 放宽到 30s（断线
// 重连的间隙由它补齐）；页面静默时不再每 5s 打一轮全表读——多个控制台
// 同开时这正是读放大主源。
let unsubRuns: (() => void) | undefined
let sseAlive = false
let pollTimer: number | undefined

function schedulePoll() {
  pollTimer = window.setTimeout(async () => {
    load(true)
    refreshDetail()
    schedulePoll()
  }, sseAlive ? 30000 : 5000)
}

onMounted(() => {
  load()
  unsubRuns = subscribeRuns(
    () => {
      sseAlive = true
      load(true)
      refreshDetail()
    },
    () => {
      // 连接失败（server 重启/502）后 EventSource 不再自动重连：回落快档
      // 轮询，直到下次收到事件再放宽
      sseAlive = false
    },
  )
  schedulePoll()
})
onUnmounted(() => {
  window.clearTimeout(pollTimer)
  unsubRuns?.()
})
</script>

<template>
  <div>
    <div class="toolbar">
      <span class="muted">应用执行与远程命令的统一审计记录（状态变化实时推送，运行中的任务实时更新）</span>
      <div style="flex: 1" />
      <el-select v-model="kindFilter" placeholder="全部类型" clearable style="width: 130px">
        <el-option label="app（应用）" value="app" />
        <el-option label="exec（命令）" value="exec" />
      </el-select>
      <el-button :icon="Refresh" @click="load()">刷新</el-button>
    </div>

    <div v-if="selection.length" class="batch-bar">
      <span>已选 <b>{{ selection.length }}</b> 条记录</span>
      <el-button size="small" type="danger" plain :icon="Delete" @click="batchDelete">批量删除</el-button>
      <el-button size="small" link @click="clearSelection">取消选择</el-button>
    </div>

    <el-card shadow="never">
      <el-table
        ref="tableRef"
        :data="runs.filter((r) => !kindFilter || r.Kind === kindFilter)"
        v-loading="loading"
        row-key="ID"
        style="width: 100%"
        @selection-change="onSelectionChange"
      >
        <el-table-column type="selection" width="44" reserve-selection />
        <el-table-column label="run" width="70" prop="ID" />
        <el-table-column label="类型" width="80">
          <template #default="{ row }">
            <el-tag size="small" :type="row.Kind === 'exec' ? 'info' : 'primary'" effect="plain">{{ row.Kind }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="应用 / 命令" min-width="150">
          <template #default="{ row }">
            {{ row.AppName || (row.Kind === 'exec' ? '远程命令' : '-') }}
            <el-tag v-if="row.Seq" size="small" type="info" effect="plain" style="margin-left: 6px">#{{ row.Seq + 1 }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="Version" label="版本" min-width="90" />
        <el-table-column label="相位" width="90">
          <template #default="{ row }">
            <el-tag v-if="row.Phase" size="small" type="info" effect="plain">{{ row.Phase }}</el-tag>
            <span v-else class="muted">-</span>
          </template>
        </el-table-column>
        <el-table-column label="操作者" width="90">
          <template #default="{ row }">{{ row.User || '-' }}</template>
        </el-table-column>
        <el-table-column label="状态" width="100">
          <template #default="{ row }">
            <el-tag :type="runStatus(row.Status)" round>{{ row.Status }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="Summary" label="摘要" min-width="200">
          <template #default="{ row }"><span class="muted">{{ row.Summary }}</span></template>
        </el-table-column>
        <el-table-column prop="StartedAt" label="开始" min-width="165">
          <template #default="{ row }"><span class="muted">{{ row.StartedAt }}</span></template>
        </el-table-column>
        <el-table-column prop="FinishedAt" label="结束" min-width="165">
          <template #default="{ row }"><span class="muted">{{ row.FinishedAt || '-' }}</span></template>
        </el-table-column>
        <el-table-column label="操作" width="130" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" @click="openRun(row)">详情</el-button>
            <el-button v-if="can('run:delete')" link type="danger" :icon="Delete" @click="removeRun(row)">删除</el-button>
          </template>
        </el-table-column>
        <template #empty><el-empty description="还没有执行记录" /></template>
      </el-table>
    </el-card>

    <!-- 任务明细 -->
    <el-drawer v-model="detailVisible" :title="`run #${detailRun?.ID} · ${detailRun?.AppName || ''}${detailRun?.Version ? '@' + detailRun.Version : ''}`" size="760px">
      <el-descriptions :column="2" border size="small" style="margin-bottom: 12px">
        <el-descriptions-item label="状态">
          <el-tag :type="runStatus(detailRun?.Status || '')" round>{{ detailRun?.Status }}</el-tag>
        </el-descriptions-item>
        <el-descriptions-item label="摘要">{{ detailRun?.Summary || '-' }}</el-descriptions-item>
        <el-descriptions-item label="目标选择器">
          <span class="muted">{{ detailRun?.Selector }}</span>
        </el-descriptions-item>
        <el-descriptions-item label="操作者">{{ detailRun?.User || '-' }}</el-descriptions-item>
        <el-descriptions-item label="时间">{{ detailRun?.StartedAt }} → {{ detailRun?.FinishedAt || '进行中' }}</el-descriptions-item>
      </el-descriptions>
      <el-table :data="detailTasks" size="small" v-loading="detailLoading">
        <el-table-column prop="Play" label="Play" min-width="110" />
        <el-table-column prop="Task" label="任务" min-width="150" />
        <el-table-column prop="Module" label="模块" width="90" />
        <el-table-column prop="Host" label="主机" min-width="110" />
        <el-table-column label="状态" width="110">
          <template #default="{ row }">
            <el-tag size="small" :type="taskStatus(row.Status)">
              {{ row.Status }}{{ row.Changed ? '·changed' : '' }}
            </el-tag>
          </template>
        </el-table-column>
      </el-table>
      <el-collapse style="margin-top: 10px">
        <el-collapse-item v-for="t in detailTasks" :key="t.ID" :title="`${t.Task} @ ${t.Host} — ${t.Status}`">
          <pre class="out">{{ t.Detail || '(无输出)' }}</pre>
        </el-collapse-item>
      </el-collapse>
    </el-drawer>
  </div>
</template>

<style scoped>
.toolbar {
  display: flex;
  align-items: center;
  margin-bottom: 12px;
}
.batch-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  background: #fef0f0;
  border: 1px solid #fde2e2;
  border-radius: 6px;
  padding: 8px 14px;
  margin-bottom: 12px;
  color: #f56c6c;
}
.out {
  background: #0f172a;
  color: #e2e8f0;
  border-radius: 6px;
  padding: 10px 12px;
  font-size: 12px;
  max-height: 320px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-all;
}
.muted {
  color: #909399;
  font-size: 12px;
}
</style>
