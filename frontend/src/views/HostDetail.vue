<script setup lang="ts">
// 主机详情独立页：基本信息、健康告警、实时指标快照（agent /metrics 的
// JSON 投影）、5 分钟聚合趋势（30 天）、setup facts、该主机的执行历史。
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { ArrowLeft, Refresh, WarningFilled } from '@element-plus/icons-vue'
import { api, type Host, type HostAlert, type HostFactsResponse, type HostTaskItem, type MetricSample, type SeriesPoint } from '../api'
import Sparkline from '../components/Sparkline.vue'

const route = useRoute()
const router = useRouter()
const hostId = computed(() => Number(route.params.id))

const host = ref<Host | null>(null)
const alerts = ref<HostAlert[]>([])
const samples = ref<MetricSample[]>([])
const facts = ref<HostFactsResponse | null>(null)
const tasks = ref<HostTaskItem[]>([])
const loading = ref(false)
const hours = ref(24)
let timer: number | undefined

async function load() {
  loading.value = true
  try {
    host.value = await api<Host>('GET', `/api/hosts/${hostId.value}`)
    await Promise.all([loadAlerts(), loadLive(), loadTasks()])
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

async function loadAlerts() {
  try {
    const list = await api<HostAlert[]>('GET', '/api/alerts')
    alerts.value = list.filter((a) => a.HostID === hostId.value)
  } catch {
    /* 静默 */
  }
}

async function loadLive() {
  try {
    samples.value = await api<MetricSample[]>('GET', `/api/hosts/${hostId.value}/metrics?format=json`)
  } catch {
    samples.value = []
  }
}

async function loadTasks() {
  try {
    tasks.value = await api<HostTaskItem[]>('GET', `/api/hosts/${hostId.value}/tasks`)
  } catch {
    tasks.value = []
  }
}

// ---- 证书换发 ----
// 后端重签（保留私钥、SAN 不变）并尝试推送 agent 热更换；agent 离线时
// 新证书已在 server 侧就位，提示重启后生效。
const renewing = ref(false)
async function renewCert() {
  try {
    await ElMessageBox.confirm(
      `重签 ${host.value?.Name || '该主机'} 的 agent 证书并热更换（保留私钥，新有效期一年）？`,
      '证书换发',
      { type: 'warning', confirmButtonText: '换证', cancelButtonText: '取消' },
    )
  } catch {
    return /* 用户取消 */
  }
  renewing.value = true
  try {
    const out = await api<{ renewed: boolean; pushed: boolean; not_after: string; message: string }>(
      'POST', `/api/hosts/${hostId.value}/renew-cert`,
    )
    ElMessage.success(out.pushed ? `证书已换发并生效（新到期 ${out.not_after}）` : out.message)
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    renewing.value = false
  }
}

// ---- 实时快照派生（与 scrapeLoop 同口径） ----
function sum(name: string, labelFilter?: { k: string; v: string }): number {
  let t = 0
  for (const s of samples.value) {
    if (s.name !== name) continue
    if (labelFilter && s.labels?.[labelFilter.k] !== labelFilter.v) continue
    t += s.value
  }
  return t
}
function first(name: string, labelFilter?: { k: string; v: string }): number | undefined {
  for (const s of samples.value) {
    if (s.name !== name) continue
    if (labelFilter && s.labels?.[labelFilter.k] !== labelFilter.v) continue
    return s.value
  }
  return undefined
}

const live = computed(() => {
  if (!samples.value.length) return null
  const mt = sum('node_memory_MemTotal_bytes')
  const ma = sum('node_memory_MemAvailable_bytes')
  const fsSize = first('node_filesystem_size_bytes', { k: 'mount', v: '/' })
  const fsAvail = first('node_filesystem_avail_bytes', { k: 'mount', v: '/' })
  return {
    cpus: samples.value.filter((s) => s.name === 'node_cpu_seconds_total' && s.labels?.mode === 'idle').length,
    load1: first('node_load1'),
    load5: first('node_load5'),
    memPct: mt > 0 && ma > 0 ? 100 * (1 - ma / mt) : undefined,
    memTotal: mt,
    memAvail: ma,
    fsPct: fsSize && fsAvail && fsSize > 0 ? 100 * (1 - fsAvail / fsSize) : undefined,
    fsSize,
    fsAvail,
    netRx: sum('node_network_receive_bytes_total'),
    netTx: sum('node_network_transmit_bytes_total'),
    boot: first('node_boot_time_seconds'),
  }
})

function fmtBytes(n: number | undefined): string {
  if (!n || n <= 0) return '-'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(i > 1 ? 1 : 0)} ${units[i]}`
}

// ---- 趋势 ----
type ChartDef = { metric: string; labels: string; title: string; unit: string; color: string; showMax?: boolean }
const charts: ChartDef[] = [
  { metric: 'cpu_usage_pct', labels: '', title: 'CPU 使用率', unit: '%', color: '#2563eb', showMax: true },
  { metric: 'mem_used_pct', labels: '', title: '内存使用率', unit: '%', color: '#7c3aed', showMax: true },
  { metric: 'fs_used_pct', labels: 'mount=/', title: '根文件系统使用率', unit: '%', color: '#059669', showMax: true },
  { metric: 'load1', labels: '', title: 'load1', unit: '', color: '#d97706', showMax: true },
  { metric: 'net_rx_bps', labels: '', title: '网络接收', unit: 'B/s', color: '#0891b2' },
  { metric: 'net_tx_bps', labels: '', title: '网络发送', unit: 'B/s', color: '#db2777' },
]
const series = ref<Record<string, SeriesPoint[]>>({})
// 自定义时间窗（datetimerange）；空 = 用快捷 hours
const customRange = ref<[Date, Date] | null>(null)

// 窗口请求序号守卫：快速切换时间窗时旧窗口的慢响应后到，不能覆盖新窗口
let seriesSeq = 0

async function loadSeries() {
  const seq = ++seriesSeq
  let qs = ''
  if (customRange.value) {
    const from = Math.floor(customRange.value[0].getTime() / 1000)
    const to = Math.floor(customRange.value[1].getTime() / 1000)
    qs = `from=${from}`
    // to 只是显示边界：series 接口按 from 起查，超出 to 的桶在前端裁掉
    for (const c of charts) {
      try {
        const pts = await api<SeriesPoint[]>(
          'GET', `/api/hosts/${hostId.value}/series?metric=${c.metric}&labels=${encodeURIComponent(c.labels)}&${qs}`,
        )
        if (seq !== seriesSeq) return
        series.value[`${c.metric}|${c.labels}`] = pts.filter((p) => p.ts <= to)
      } catch {
        if (seq !== seriesSeq) return
        series.value[`${c.metric}|${c.labels}`] = []
      }
    }
    return
  }
  for (const c of charts) {
    try {
      const pts = await api<SeriesPoint[]>(
        'GET', `/api/hosts/${hostId.value}/series?metric=${c.metric}&labels=${encodeURIComponent(c.labels)}&hours=${hours.value}`,
      )
      if (seq !== seriesSeq) return
      series.value[`${c.metric}|${c.labels}`] = pts
    } catch {
      if (seq !== seriesSeq) return
      series.value[`${c.metric}|${c.labels}`] = []
    }
  }
}

function hoursChanged() {
  customRange.value = null
  void loadSeries()
}

function customChanged(v: [Date, Date] | null) {
  if (v) void loadSeries()
}

// ---- facts ----
const factsLoading = ref(false)

async function loadFacts() {
  factsLoading.value = true
  try {
    facts.value = await api<HostFactsResponse>('GET', `/api/hosts/${hostId.value}/facts`)
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    factsLoading.value = false
  }
}

onMounted(() => {
  void load()
  void loadSeries()
  void loadFacts()
  timer = window.setInterval(() => {
    loadLive()
    loadAlerts()
  }, 30_000)
})
onUnmounted(() => window.clearInterval(timer))

function statusType(s: string | undefined): 'success' | 'danger' | 'info' {
  return s === 'online' ? 'success' : s === 'offline' ? 'danger' : 'info'
}
function taskStatus(s: string): 'success' | 'info' | 'danger' {
  return s === 'ok' ? 'success' : s === 'skipped' ? 'info' : 'danger'
}
</script>

<template>
  <div v-loading="loading">
    <div class="toolbar">
      <el-button :icon="ArrowLeft" @click="router.push('/hosts')">返回列表</el-button>
      <h2 class="title">
        {{ host?.Name || `主机 #${hostId}` }}
        <el-tag v-if="host" :type="statusType(host.Status)" round style="margin-left: 8px">{{ host.Status }}</el-tag>
        <el-popover v-for="a in alerts" :key="a.Kind" placement="bottom" :width="360" trigger="hover">
          <template #reference>
            <el-icon
              :size="18" class="alert-dot"
              :color="a.Level === 'crit' ? '#f56c6c' : '#e6a23c'"
            ><WarningFilled /></el-icon>
          </template>
          <div class="alert-item">
            <el-tag size="small" :type="a.Level === 'crit' ? 'danger' : 'warning'" round>
              {{ a.Level === 'crit' ? '严重' : '警告' }}
            </el-tag>
            {{ a.Detail }}
            <span class="muted">（{{ a.UpdatedAt }}）</span>
          </div>
        </el-popover>
      </h2>
      <div style="flex: 1" />
      <el-button :loading="renewing" @click="renewCert">换证</el-button>
      <el-button :icon="Refresh" @click="load(); loadFacts()">刷新</el-button>
    </div>

    <!-- 告警条 -->
    <el-alert
      v-for="a in alerts" :key="a.Kind"
      :type="a.Level === 'crit' ? 'error' : 'warning'" :closable="false" show-icon style="margin-bottom: 10px"
      :title="`【${a.Level === 'crit' ? '严重' : '警告'}】${a.Detail}（更新于 ${a.UpdatedAt}）`"
    />

    <!-- 基本信息 -->
    <el-card shadow="never" class="block">
      <template #header><b>基本信息</b></template>
      <el-descriptions :column="4" size="small" border>
        <el-descriptions-item label="地址">{{ host?.Address }}</el-descriptions-item>
        <el-descriptions-item label="agent 端口">{{ host?.AgentPort }}</el-descriptions-item>
        <el-descriptions-item label="最近在线">{{ host?.LastSeenAt || '-' }}</el-descriptions-item>
        <el-descriptions-item label="加入时间">{{ host?.CreatedAt }}</el-descriptions-item>
        <el-descriptions-item label="池" :span="2">{{ (host?.Pools || []).join('、') || '-' }}</el-descriptions-item>
        <el-descriptions-item label="组" :span="2">{{ (host?.Groups || []).join('、') || '-' }}</el-descriptions-item>
      </el-descriptions>
    </el-card>

    <!-- 实时快照 -->
    <el-card shadow="never" class="block">
      <template #header>
        <div class="card-head">
          <b>实时快照</b>
          <span class="muted">agent /metrics 当前值（30s 自动刷新）；Prometheus 抓取口：GET /api/hosts/{{ hostId }}/metrics（basic auth）</span>
        </div>
      </template>
      <el-descriptions v-if="live" :column="4" size="small" border>
        <el-descriptions-item label="CPU 核数">{{ live.cpus || '-' }}</el-descriptions-item>
        <el-descriptions-item label="load1">{{ live.load1?.toFixed(2) ?? '-' }}</el-descriptions-item>
        <el-descriptions-item label="内存">
          {{ live.memPct !== undefined ? live.memPct.toFixed(1) + '%' : '-' }}
          <span class="muted">（可用 {{ fmtBytes(live.memAvail) }} / {{ fmtBytes(live.memTotal) }}）</span>
        </el-descriptions-item>
        <el-descriptions-item label="根文件系统">
          {{ live.fsPct !== undefined ? live.fsPct.toFixed(1) + '%' : '-' }}
          <span class="muted">（可用 {{ fmtBytes(live.fsAvail) }} / {{ fmtBytes(live.fsSize) }}）</span>
        </el-descriptions-item>
        <el-descriptions-item label="累计接收">{{ fmtBytes(live.netRx) }}</el-descriptions-item>
        <el-descriptions-item label="累计发送">{{ fmtBytes(live.netTx) }}</el-descriptions-item>
        <el-descriptions-item label="启动时间" :span="2">{{ live.boot ? new Date(live.boot * 1000).toLocaleString() : '-' }}</el-descriptions-item>
      </el-descriptions>
      <el-empty v-else description="实时指标不可用（agent 离线或版本过旧）" :image-size="48" />
    </el-card>

    <!-- 趋势 -->
    <el-card shadow="never" class="block">
      <template #header>
        <div class="card-head">
          <b>趋势（5 分钟聚合，保留 30 天）</b>
          <div class="range-picker">
            <el-radio-group v-model="hours" size="small" @change="hoursChanged">
              <el-radio-button :value="6">6h</el-radio-button>
              <el-radio-button :value="24">24h</el-radio-button>
              <el-radio-button :value="168">7d</el-radio-button>
              <el-radio-button :value="720">30d</el-radio-button>
            </el-radio-group>
            <el-date-picker
              v-model="customRange" type="datetimerange" size="small" style="width: 340px"
              range-separator="→" start-placeholder="开始时间" end-placeholder="结束时间"
              format="MM-DD HH:mm" :clearable="true" @change="customChanged"
            />
          </div>
        </div>
      </template>
      <div class="charts">
        <div v-for="c in charts" :key="c.metric + c.labels" class="chart-item">
          <div class="chart-title">{{ c.title }}</div>
          <Sparkline
            :points="series[`${c.metric}|${c.labels}`] || []" :unit="c.unit" :color="c.color"
            :width="560" :height="150" :show-max="c.showMax"
          />
        </div>
      </div>
    </el-card>

    <!-- facts -->
    <el-card shadow="never" class="block" v-loading="factsLoading">
      <template #header><b>系统信息（setup 采集）</b></template>
      <template v-if="facts?.facts">
        <el-descriptions :column="4" size="small" border>
          <el-descriptions-item label="主机名">{{ facts.facts.hostname || '-' }}</el-descriptions-item>
          <el-descriptions-item label="内核">{{ facts.facts.kernel || '-' }}</el-descriptions-item>
          <el-descriptions-item label="系统">{{ facts.facts.os?.name || facts.facts.os?.id || '-' }} {{ facts.facts.os?.version }}</el-descriptions-item>
          <el-descriptions-item label="家族">
            <el-tag size="small" effect="plain">{{ facts.facts.os?.family || '-' }}</el-tag>
          </el-descriptions-item>
          <el-descriptions-item label="架构">{{ facts.facts.arch || '-' }}</el-descriptions-item>
          <el-descriptions-item label="默认 IPv4">{{ facts.facts.default_ipv4 || '-' }}</el-descriptions-item>
          <el-descriptions-item label="agent 版本">{{ facts.probe.version || '-' }}</el-descriptions-item>
          <el-descriptions-item label="证书到期">{{ facts.probe.cert_not_after || '-' }}</el-descriptions-item>
        </el-descriptions>
      </template>
      <el-alert v-else type="info" :closable="false" :title="facts?.error || '点击刷新采集 setup facts'" />
    </el-card>

    <!-- 执行历史 -->
    <el-card shadow="never" class="block">
      <template #header><b>该主机的执行历史（最近 30 条任务）</b></template>
      <el-table :data="tasks" size="small">
        <el-table-column label="时间" width="170">
          <template #default="{ row }"><span class="muted">{{ row.StartAt }}</span></template>
        </el-table-column>
        <el-table-column label="类型" width="70">
          <template #default="{ row }">
            <el-tag size="small" :type="row.Kind === 'exec' ? 'info' : 'primary'" effect="plain">{{ row.Kind }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="AppName" label="应用" min-width="110">
          <template #default="{ row }">{{ row.AppName || '远程命令' }}</template>
        </el-table-column>
        <el-table-column prop="Task" label="任务" min-width="140" />
        <el-table-column prop="Module" label="模块" width="90" />
        <el-table-column label="状态" width="100">
          <template #default="{ row }">
            <el-tag size="small" :type="taskStatus(row.Status)">{{ row.Status }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="输出" min-width="200">
          <template #default="{ row }">
            <el-popover placement="left" :width="480" trigger="hover">
              <template #reference><span class="muted out-line">{{ (row.Detail || '').slice(0, 80) || '-' }}</span></template>
              <pre class="out">{{ row.Detail || '(无输出)' }}</pre>
            </el-popover>
          </template>
        </el-table-column>
        <template #empty><el-empty description="该主机还没有执行记录" :image-size="48" /></template>
      </el-table>
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
.title {
  margin: 0;
  font-size: 18px;
  display: flex;
  align-items: center;
  gap: 8px;
}
.alert-dot {
  margin-left: 2px;
}
.block {
  margin-bottom: 16px;
}
.card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 8px;
}
.range-picker {
  display: flex;
  align-items: center;
  gap: 10px;
}
.charts {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(600px, 1fr));
  gap: 12px 20px;
}
.chart-item {
  border: 1px solid #ebeef5;
  border-radius: 8px;
  padding: 8px 12px;
}
.chart-title {
  font-size: 13px;
  color: #606266;
  margin-bottom: 4px;
}
.muted {
  color: #909399;
  font-size: 12px;
}
.out {
  background: #0f172a;
  color: #e2e8f0;
  border-radius: 6px;
  padding: 8px 10px;
  font-size: 12px;
  max-height: 300px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-all;
  margin: 0;
}
.out-line {
  display: inline-block;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
