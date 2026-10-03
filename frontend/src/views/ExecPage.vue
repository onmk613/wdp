<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { CaretRight, Refresh, Search } from '@element-plus/icons-vue'
import { can } from '../auth'
import {
  api, subscribeExecStream, type ExecHostResult, type Host, type RunTask,
} from '../api'
import TailPre from '../components/TailPre.vue'

const hosts = ref<Host[]>([])
const loading = ref(false)
const search = ref('')
const selection = ref<Host[]>([])
const tableRef = ref()

const script = ref('uname -a')
const timeoutSec = ref(120)
const running = ref(false)
// pending=true 的条目是尚未完成的主机占位（spinner），事件到达后原位填充
type LiveResult = ExecHostResult & { pending?: boolean }
const results = ref<LiveResult[]>([])
const runID = ref(0)
// 已完成计数（header 徽标）：ok/failed 之和，随事件推进
const doneCount = computed(() => results.value.filter((r) => !r.pending).length)
// 目标按执行权限裁剪（restricted=true 时提示；数据源是 /api/exec/targets
// 而非台账全量——资源显示与执行权限同口径）
const restricted = ref(false)

const pools = computed(() => [...new Set(hosts.value.flatMap((h) => h.pools || []))])
const poolFilter = ref('')
// 搜索与池过滤都是纯前端过滤（数据源就一页 /api/exec/targets），不随击键发请求
const shown = computed(() => {
  let list = hosts.value
  const q = search.value.trim().toLowerCase()
  if (q) list = list.filter((h) => h.name.toLowerCase().includes(q) || h.address.toLowerCase().includes(q))
  if (poolFilter.value) list = list.filter((h) => (h.pools || []).includes(poolFilter.value))
  return list
})

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    const r = await api<{ hosts: Host[]; restricted: boolean }>('GET', '/api/exec/targets')
    hosts.value = r.hosts || []
    restricted.value = !!r.restricted
  } catch (e) {
    if (!silent) ElMessage.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

function onSelectionChange(rows: Host[]) {
  selection.value = rows
}

// 流订阅与兜底轮询的清理句柄（组件卸载 / 下一次执行前释放）
let stopStream: (() => void) | null = null
let pollTimer = 0

function finishRun(ok: number, failed: number) {
  running.value = false
  stopStream?.()
  stopStream = null
  window.clearTimeout(pollTimer)
  if (failed === 0) ElMessage.success(`${ok} 台执行成功`)
  else ElMessage.warning(`${ok} 成功 / ${failed} 失败`)
}

// 以落库任务明细补齐结果（done 后仍有 pending 占位 = 流上丢了事件；
// 或流断开后的兜底轮询收敛）。Detail 是截断快照，展示口径从简
async function fillFromStore(id: number, final = false) {
  try {
    const r = await api<{ run: { status: string }; tasks: RunTask[] }>('GET', `/api/runs/${id}`)
    const byName = new Map(r.tasks.map((t) => [t.host, t]))
    for (const [i, res] of results.value.entries()) {
      if (!res.pending) continue
      const t = byName.get(res.name)
      if (!t) continue
      results.value[i] = {
        id: res.id, name: res.name,
        code: t.status === 'ok' ? 0 : 1,
        stdout: t.detail || '', stderr: '',
        err: t.status === 'unreachable' ? t.detail : '',
      }
    }
    if (final) {
      const ok = r.tasks.filter((t) => t.status === 'ok').length
      finishRun(ok, r.tasks.length - ok)
    }
  } catch { /* 兜底失败：保持现状等下一轮 */ }
}

// 兜底轮询：SSE 断开（HTTP 错误不自动重连）时按 3s 收敛到落库结果
function pollFallback(id: number) {
  window.clearTimeout(pollTimer)
  pollTimer = window.setTimeout(async () => {
    if (!running.value) return
    try {
      const r = await api<{ status: string }>('GET', `/api/runs/${id}`)
      if (r.status !== 'running') {
        await fillFromStore(id, true)
        return
      }
    } catch { /* 网络故障：下一轮再试 */ }
    pollFallback(id)
  }, 3000)
}

async function run() {
  if (!selection.value.length) {
    ElMessage.warning('先勾选目标主机')
    return
  }
  if (!script.value.trim()) {
    ElMessage.warning('脚本不能为空')
    return
  }
  const targets = [...selection.value]
  running.value = true
  // 选择序占位：全部 pending，事件到达原位填充（大批量时逐台可见）
  results.value = targets.map((h) => ({ id: h.id, name: h.name, code: 0, stdout: '', stderr: '', pending: true }))
  try {
    const r = await api<{ run_id: number }>('POST', '/api/exec', {
      host_ids: targets.map((h) => h.id), script: script.value, timeout_sec: timeoutSec.value,
    })
    runID.value = r.run_id
    stopStream?.()
    stopStream = subscribeExecStream(
      r.run_id,
      (h) => {
        const idx = results.value.findIndex((x) => x.id === h.result.id || (h.result.id === 0 && x.name === h.result.name))
        const filled: LiveResult = { ...h.result }
        if (idx >= 0) results.value[idx] = filled
        else results.value.push(filled)
      },
      (d) => {
        // done 后仍有占位（订阅通道满丢事件）：按落库明细补齐再收尾
        if (results.value.some((x) => x.pending)) void fillFromStore(r.run_id).then(() => finishRun(d.ok, d.failed))
        else finishRun(d.ok, d.failed)
      },
      () => { if (running.value) pollFallback(r.run_id) },
    )
  } catch (e) {
    ElMessage.error((e as Error).message)
    running.value = false
  }
}

onMounted(() => load())
onBeforeUnmount(() => {
  stopStream?.()
  window.clearTimeout(pollTimer)
})
</script>

<template>
  <div>
    <el-alert v-if="restricted" type="info" :closable="false" show-icon style="margin-bottom: 10px"
      title="目标主机已按你的执行权限（run:execute 作用域）裁剪——清单之外的主机不可执行。" />
    <div class="toolbar">
      <el-input v-model="search" placeholder="搜索主机" :prefix-icon="Search" clearable style="width: 240px" />
      <el-select v-model="poolFilter" placeholder="按池过滤" clearable style="width: 160px">
        <el-option v-for="p in pools" :key="p" :label="p" :value="p" />
      </el-select>
      <div style="flex: 1" />
      <span class="muted">已选 {{ selection.length }} 台</span>
      <el-button :icon="Refresh" @click="load()">刷新</el-button>
    </div>

    <div class="grid">
      <div class="left">
        <el-card shadow="never">
          <el-table
            ref="tableRef"
            :data="shown"
            v-loading="loading"
            height="420"
            @selection-change="onSelectionChange"
          >
            <el-table-column type="selection" width="44" />
            <el-table-column label="状态" width="84">
              <template #default="{ row }">
                <el-tag :type="row.status === 'online' ? 'success' : row.status === 'offline' ? 'danger' : 'info'" round>
                  {{ row.status }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column prop="name" label="主机名" min-width="120" />
            <el-table-column label="地址" min-width="150">
              <template #default="{ row }">{{ row.address }}:{{ row.agent_port }}</template>
            </el-table-column>
            <el-table-column label="池" min-width="100">
              <template #default="{ row }">
                <el-tag v-for="p in row.pools || []" :key="p" type="warning" effect="plain" size="small">{{ p }}</el-tag>
              </template>
            </el-table-column>
          </el-table>
        </el-card>
      </div>
      <div class="right">
        <el-card shadow="never">
          <template #header><span>命令 / 脚本（POSIX sh）</span></template>
          <el-input v-model="script" type="textarea" :rows="9" placeholder="echo hello" spellcheck="false"
            style="font-family: ui-monospace, Menlo, monospace" />
          <div class="run-row">
            <span class="muted">单主机超时</span>
            <el-input-number v-model="timeoutSec" :min="5" :max="3600" :step="30" size="small" />
            <span class="muted">秒</span>
            <div style="flex: 1" />
            <el-button v-if="can('run:execute')" type="primary" :icon="CaretRight" :loading="running"
              :disabled="!selection.length" @click="run">
              执行（{{ selection.length }} 台）
            </el-button>
          </div>
        </el-card>
      </div>
    </div>

    <el-card v-if="results.length" shadow="never" class="block">
      <template #header>
        <span>执行结果</span>
        <span class="muted" style="margin-left: 12px">
          run #{{ runID }}（已记录到「执行记录」，可回看输出）<template v-if="running"> · {{ doneCount }}/{{ results.length }} 台完成</template>
        </span>
      </template>
      <el-collapse>
        <el-collapse-item v-for="r in results" :key="r.id">
          <template #title>
            <el-tag v-if="r.pending" type="info" size="small" style="margin-right: 8px">运行中…</el-tag>
            <el-tag v-else :type="r.err || r.code !== 0 ? 'danger' : 'success'" size="small" style="margin-right: 8px">
              {{ r.err ? 'unreachable' : 'rc=' + r.code }}
            </el-tag>
            <b>{{ r.name }}</b>
            <span v-if="!r.pending" class="muted" style="margin-left: 10px">
              stdout {{ (r.stdout || '').length }}B · stderr {{ (r.stderr || '').length }}B
            </span>
          </template>
          <div v-if="r.pending" class="muted" style="padding: 6px 0">等待执行完成…</div>
          <div v-else-if="r.err" class="err">{{ r.err }}</div>
          <template v-else>
            <div class="out-label">stdout</div>
            <TailPre v-if="r.stdout" :text="r.stdout" />
            <div v-else class="muted" style="padding: 4px 0">(stdout 无输出)</div>
            <template v-if="r.stderr">
              <div class="out-label">stderr</div>
              <TailPre :text="r.stderr" class="err-out" />
            </template>
          </template>
        </el-collapse-item>
      </el-collapse>
    </el-card>
  </div>
</template>

<style scoped>
.toolbar {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
}
.grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 16px;
}
@media (max-width: 1000px) {
  .grid {
    grid-template-columns: 1fr;
  }
}
.run-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 10px;
}
.out-label {
  font-size: 12px;
  color: #909399;
  margin: 6px 0 2px;
}
.err {
  color: #f56c6c;
  font-size: 13px;
  padding: 6px 0;
}
.err-out {
  border-left: 3px solid #f56c6c;
}
.block {
  margin-top: 16px;
}
.muted {
  color: #909399;
  font-size: 12px;
}
</style>
