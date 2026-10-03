<script setup lang="ts">
// 执行记录：应用执行（kind=app，多应用按序各一行 run）、远程命令
// （kind=exec）与 agent 升级（kind=upgrade，每台主机一行）的统一审计
// 视图。列表 SSE 事件驱动即时刷新；轮询兜底（收到事件前 5s、
// 收到后放缓到 30s 慢档）。点击行查看任务明细与逐任务输出。
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { can } from '../auth'
import { Delete, Refresh } from '@element-plus/icons-vue'
import { api, subscribeRuns, type Run, type RunTask } from '../api'
import { runStatus, taskStatus, fmtTime, selectorLabel } from '../lib/format'
import type { PagedResp } from '../lib/usePaging'
import Pager from '../components/Pager.vue'
import TailPre from '../components/TailPre.vue'

const runs = ref<Run[]>([])
const loading = ref(false)
// 分页 + 类型过滤都下推服务端（此前 limit=100 全量取回再客户端过滤，
// 数据多时既慢又只见前 100 条）
const kindFilter = ref('')
const page = ref(1)
const pageSize = ref(50)
const total = ref(0)
const filteredRuns = computed(() => runs.value) // 兼容既有模板引用；过滤已服务端化

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    const params = new URLSearchParams({ page: String(page.value), page_size: String(pageSize.value) })
    if (kindFilter.value) params.set('kind', kindFilter.value)
    const r = await api<PagedResp<Run>>('GET', `/api/runs?${params}`)
    runs.value = r.items
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

// ---- 勾选与删除 ----
const selection = ref<Run[]>([])
const tableRef = ref()

function onSelectionChange(rows: Run[]) {
  selection.value = rows
}

function clearSelection() {
  tableRef.value?.clearSelection()
}

// run 的显示名：升级 run 的 app_name 即主机名（后端已填）；兜底文案只对
// exec 与历史空行生效
function runLabel(r: Run | null): string {
  if (!r) return ''
  return r.app_name || (r.kind === 'upgrade' ? 'agent 升级' : '远程命令')
}

async function removeRun(r: Run) {
  try {
    await ElMessageBox.confirm(`删除执行记录 run #${r.id}（${runLabel(r)}）？任务明细一并删除，不可恢复。`, '删除记录', {
      type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    await api('DELETE', `/api/runs/${r.id}`)
    ElMessage.success('已删除')
    if (detailRun.value?.id === r.id) detailVisible.value = false
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
      ids: selection.value.map((x) => x.id),
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
const detailScript = ref('')
const detailScriptSHA = ref('')
const detailLoading = ref(false)

// 任务明细状态过滤：百台执行的任务表按状态一键收窄，找失败/不可达
// 主机不用滚动翻找。计数随任务落库实时推进
type TaskFilter = 'all' | 'failed' | 'unreachable' | 'skipped'
const taskFilter = ref<TaskFilter>('all')
const taskCounts = computed(() => {
  const c = { all: detailTasks.value.length, failed: 0, unreachable: 0, skipped: 0 }
  for (const t of detailTasks.value) {
    if (t.status === 'failed') c.failed++
    else if (t.status === 'unreachable') c.unreachable++
    else if (t.status === 'skipped') c.skipped++
  }
  return c
})
const filteredTasks = computed(() =>
  taskFilter.value === 'all' ? detailTasks.value : detailTasks.value.filter((t) => t.status === taskFilter.value),
)

async function openRun(r: Run) {
  detailVisible.value = true
  detailLoading.value = true
  detailRun.value = r
  detailTasks.value = []
  detailScript.value = ''
  detailScriptSHA.value = ''
  taskFilter.value = 'all'
  try {
    const d = await api<{ run: Run; tasks: RunTask[]; script?: string; script_sha256?: string }>('GET', `/api/runs/${r.id}`)
    // 竞态守卫：等待期间用户可能已点开另一条 run（慢响应后到会整体
    // 替换抽屉内容，且 refreshDetail 之后一直刷错的那条）
    if (detailRun.value?.id !== r.id) return
    detailRun.value = d.run
    detailTasks.value = d.tasks
    detailScript.value = d.script || ''
    detailScriptSHA.value = d.script_sha256 || ''
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    detailLoading.value = false
  }
}

// 详情打开时跟随刷新（非终态 run 实时更新：running/queued/cancelling 都会推进）
const RUN_TERMINAL = new Set(['succeeded', 'failed', 'cancelled'])

// cancelRun 取消进行中的 run（在途任务执行完后生效，已执行结果保留）
async function cancelRun(r: Run) {
  try {
    await ElMessageBox.confirm(
      `取消 run #${r.id}（${runLabel(r)}）？在途任务会执行完，已执行任务的结果保留，幂等应用可重新发起续跑。`,
      '取消执行', { type: 'warning', confirmButtonText: '取消执行', cancelButtonText: '再想想' },
    )
  } catch {
    return
  }
  try {
    await api('POST', `/api/runs/${r.id}/cancel`, {})
    ElMessage.success('已请求取消（在途任务执行完后生效）')
    refreshDetail()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}
async function refreshDetail() {
  if (!detailVisible.value || !detailRun.value) return
  if (RUN_TERMINAL.has(detailRun.value.status)) return
  const id = detailRun.value.id
  try {
    const d = await api<{ run: Run; tasks: RunTask[]; script?: string; script_sha256?: string }>('GET', `/api/runs/${id}`)
    // 竞态守卫（与 openRun 同口径）：慢响应回来时用户可能已点开另一条
    // run——迟到响应整体覆盖抽屉内容后，后续轮询会一直刷错的那条
    if (detailRun.value?.id !== id) return
    detailRun.value = d.run
    detailTasks.value = d.tasks
    detailScript.value = d.script || ''
    detailScriptSHA.value = d.script_sha256 || ''
  } catch {
    /* 静默 */
  }
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
        <el-option label="upgrade（升级）" value="upgrade" />
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
        :data="filteredRuns"
        v-loading="loading"
        row-key="id"
        style="width: 100%"
        @selection-change="onSelectionChange"
      >
        <el-table-column type="selection" width="44" reserve-selection />
        <el-table-column label="run" width="70" prop="id" />
        <el-table-column label="类型" width="80">
          <template #default="{ row }">
            <el-tag size="small" :type="row.kind === 'exec' ? 'info' : row.kind === 'upgrade' ? 'warning' : 'primary'" effect="plain">{{ row.kind }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="应用 / 命令" min-width="150">
          <template #default="{ row }">
            {{ row.app_name || (row.kind === 'exec' ? '远程命令' : row.kind === 'upgrade' ? 'agent 升级' : '-') }}
            <el-tag v-if="row.seq" size="small" type="info" effect="plain" style="margin-left: 6px">#{{ row.seq + 1 }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="version" label="版本" min-width="90" />
        <el-table-column label="相位" width="90">
          <template #default="{ row }">
            <el-tag v-if="row.phase" size="small" type="info" effect="plain">{{ row.phase }}</el-tag>
            <span v-else class="muted">-</span>
          </template>
        </el-table-column>
        <el-table-column label="操作者" width="90">
          <template #default="{ row }">{{ row.user || '-' }}</template>
        </el-table-column>
        <el-table-column label="状态" width="100">
          <template #default="{ row }">
            <el-tag :type="runStatus(row.status)" round>{{ row.status }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="summary" label="摘要" min-width="200">
          <template #default="{ row }"><span class="muted">{{ row.summary }}</span></template>
        </el-table-column>
        <el-table-column prop="started_at" label="开始" min-width="165">
          <template #default="{ row }"><span class="muted">{{ fmtTime(row.started_at) }}</span></template>
        </el-table-column>
        <el-table-column prop="finished_at" label="结束" min-width="165">
          <template #default="{ row }"><span class="muted">{{ fmtTime(row.finished_at) }}</span></template>
        </el-table-column>
        <el-table-column label="操作" width="130" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" @click="openRun(row)">详情</el-button>
            <el-button v-if="can('run:delete')" link type="danger" :icon="Delete" @click="removeRun(row)">删除</el-button>
          </template>
        </el-table-column>
        <template #empty><el-empty description="还没有执行记录" /></template>
      </el-table>
      <Pager :page="page" :page-size="pageSize" :total="total" @update:page="(v) => { page = v; load(true) }" @update:page-size="(v) => { pageSize = v; load(true) }" />
    </el-card>

    <!-- 任务明细 -->
    <!-- destroy-on-close：关闭即卸载内容。曾出现关闭动画被打断（标签页
         后台化/渲染节流）时 leave 过渡不完结、遮罩残留且整页点击失效，
         只能刷新恢复——销毁式关闭把残留窗口压到最小 -->
    <el-drawer v-model="detailVisible" :title="detailRun ? `run #${detailRun.id} · ${runLabel(detailRun)}${detailRun.version ? '@' + detailRun.version : ''}` : '执行详情'" size="760px" destroy-on-close>
      <el-descriptions :column="2" border size="small" style="margin-bottom: 12px">
        <el-descriptions-item label="状态">
          <el-tag :type="runStatus(detailRun?.status || '')" round>{{ detailRun?.status }}</el-tag>
        </el-descriptions-item>
        <el-descriptions-item label="摘要">{{ detailRun?.summary || '-' }}</el-descriptions-item>
        <el-descriptions-item label="目标选择器">
          <span class="muted">{{ selectorLabel(detailRun?.selector) }}</span>
        </el-descriptions-item>
        <el-descriptions-item label="操作者">{{ detailRun?.user || '-' }}</el-descriptions-item>
        <el-descriptions-item label="时间">{{ fmtTime(detailRun?.started_at) }} → {{ detailRun?.finished_at ? fmtTime(detailRun?.finished_at) : '进行中' }}</el-descriptions-item>
      </el-descriptions>
      <div v-if="detailRun && !RUN_TERMINAL.has(detailRun.status) && can('run:execute')" style="margin-bottom: 10px">
        <el-button size="small" type="warning" plain @click="cancelRun(detailRun)">取消执行</el-button>
      </div>
      <div class="task-filter">
        <el-radio-group v-model="taskFilter" size="small">
          <el-radio-button value="all">全部 {{ taskCounts.all }}</el-radio-button>
          <el-radio-button value="failed">失败 {{ taskCounts.failed }}</el-radio-button>
          <el-radio-button value="unreachable">不可达 {{ taskCounts.unreachable }}</el-radio-button>
          <el-radio-button value="skipped">跳过 {{ taskCounts.skipped }}</el-radio-button>
        </el-radio-group>
      </div>
      <el-table :data="filteredTasks" size="small" v-loading="detailLoading">
        <el-table-column prop="play" label="Play" min-width="110" />
        <el-table-column prop="task" label="任务" min-width="150" />
        <el-table-column prop="module" label="模块" width="90" />
        <el-table-column prop="host" label="主机" min-width="110" />
        <el-table-column label="状态" width="110">
          <template #default="{ row }">
            <el-tag size="small" :type="taskStatus(row.status)">
              {{ row.status }}{{ row.changed ? '·changed' : '' }}
            </el-tag>
          </template>
        </el-table-column>
      </el-table>
      <el-collapse v-if="detailRun?.kind === 'exec' && detailScript" style="margin-top: 10px">
        <el-collapse-item title="执行脚本（审计证据，超长已截断）">
          <pre class="out">{{ detailScript }}</pre>
          <div v-if="detailScriptSHA" class="muted" style="margin-top: 4px; font-size: 12px">
            sha256: {{ detailScriptSHA }}
          </div>
        </el-collapse-item>
      </el-collapse>
      <el-collapse style="margin-top: 10px">
        <el-collapse-item v-for="t in filteredTasks" :key="t.id" :title="`${t.task} @ ${t.host} — ${t.status}`">
          <TailPre v-if="t.detail" :text="t.detail" />
          <div v-else class="muted">(无输出)</div>
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
.task-filter {
  margin-bottom: 8px;
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
