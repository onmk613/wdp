<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { CaretRight, Refresh, Search } from '@element-plus/icons-vue'
import { can } from '../auth'
import { api, type ExecHostResult, type Host } from '../api'

const hosts = ref<Host[]>([])
const loading = ref(false)
const search = ref('')
const selection = ref<Host[]>([])
const tableRef = ref()

const script = ref('uname -a')
const timeoutSec = ref(120)
const running = ref(false)
const results = ref<ExecHostResult[]>([])
const runID = ref(0)
// 目标按执行权限裁剪（restricted=true 时提示；数据源是 /api/exec/targets
// 而非台账全量——资源显示与执行权限同口径）
const restricted = ref(false)

const pools = computed(() => [...new Set(hosts.value.flatMap((h) => h.Pools || []))])
const poolFilter = ref('')
// 搜索与池过滤都是纯前端过滤（数据源就一页 /api/exec/targets），不随击键发请求
const shown = computed(() => {
  let list = hosts.value
  const q = search.value.trim().toLowerCase()
  if (q) list = list.filter((h) => h.Name.toLowerCase().includes(q) || h.Address.toLowerCase().includes(q))
  if (poolFilter.value) list = list.filter((h) => (h.Pools || []).includes(poolFilter.value))
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

async function run() {
  if (!selection.value.length) {
    ElMessage.warning('先勾选目标主机')
    return
  }
  if (!script.value.trim()) {
    ElMessage.warning('脚本不能为空')
    return
  }
  running.value = true
  results.value = []
  try {
    const r = await api<{ run_id: number; ok: number; failed: number; results: ExecHostResult[] }>(
      'POST', '/api/exec',
      { host_ids: selection.value.map((h) => h.ID), script: script.value, timeout_sec: timeoutSec.value },
    )
    runID.value = r.run_id
    results.value = r.results
    if (r.failed === 0) ElMessage.success(`${r.ok} 台执行成功`)
    else ElMessage.warning(`${r.ok} 成功 / ${r.failed} 失败`)
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    running.value = false
  }
}

onMounted(() => load())
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
                <el-tag :type="row.Status === 'online' ? 'success' : row.Status === 'offline' ? 'danger' : 'info'" round>
                  {{ row.Status }}
                </el-tag>
              </template>
            </el-table-column>
            <el-table-column prop="Name" label="主机名" min-width="120" />
            <el-table-column label="地址" min-width="150">
              <template #default="{ row }">{{ row.Address }}:{{ row.AgentPort }}</template>
            </el-table-column>
            <el-table-column label="池" min-width="100">
              <template #default="{ row }">
                <el-tag v-for="p in row.Pools || []" :key="p" type="warning" effect="plain" size="small">{{ p }}</el-tag>
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
        <span class="muted" style="margin-left: 12px">run #{{ runID }}（记录已入审计）</span>
      </template>
      <el-collapse>
        <el-collapse-item v-for="r in results" :key="r.id">
          <template #title>
            <el-tag :type="r.err || r.code !== 0 ? 'danger' : 'success'" size="small" style="margin-right: 8px">
              {{ r.err ? 'unreachable' : 'rc=' + r.code }}
            </el-tag>
            <b>{{ r.name }}</b>
            <span class="muted" style="margin-left: 10px">
              stdout {{ (r.stdout || '').length }}B · stderr {{ (r.stderr || '').length }}B
            </span>
          </template>
          <div v-if="r.err" class="err">{{ r.err }}</div>
          <template v-else>
            <div class="out-label">stdout</div>
            <pre class="out">{{ r.stdout || '(stdout 无输出)' }}</pre>
            <template v-if="r.stderr">
              <div class="out-label">stderr</div>
              <pre class="out err-out">{{ r.stderr }}</pre>
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
.out {
  background: #0f172a;
  color: #e2e8f0;
  border-radius: 6px;
  padding: 10px 12px;
  font-size: 12px;
  max-height: 300px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-all;
}
.block {
  margin-top: 16px;
}
.muted {
  color: #909399;
  font-size: 12px;
}
</style>
