<script setup lang="ts">
// 应用列表：前面勾选（批量删除），点击应用名/展开箭头在行下方展开版本明细。
// 版本行带「修改（基于此版本编辑）」「设为默认（latest）」「删除」；
// 默认版本行标记 latest 徽标。应用级操作全部收进展开区，无右侧操作列。
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Delete, Download, EditPen, PriceTag, Refresh, Star, Upload } from '@element-plus/icons-vue'
import { api, upload, appDownloadURL, type App, type AppVersion, type GroupEntry, type LabelDef, type Pool } from '../api'
import LabelRows from '../components/LabelRows.vue'
import { can } from '../auth'
import { parseLabels } from '../lib/format'

const router = useRouter()

const apps = ref<App[]>([])
const loading = ref(false)
const pools = ref<Pool[]>([])
const groups = ref<GroupEntry[]>([])
const labels = ref<LabelDef[]>([])

// 展开行的版本缓存：appId → 版本列表（展开时按需加载）
const versions = reactive<Record<number, AppVersion[]>>({})
const loadingVersions = reactive<Record<number, boolean>>({})
const expanded = ref<number[]>([])
const tableRef = ref()

// ---- 列表 ----
async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    ;[apps.value, pools.value, groups.value, labels.value] = await Promise.all([
      api<App[]>('GET', '/api/apps'),
      api<Pool[]>('GET', '/api/pools'),
      api<GroupEntry[]>('GET', '/api/groups'),
      api<LabelDef[]>('GET', '/api/labels'),
    ])
    for (const id of expanded.value) loadVersions(id, true)
  } catch (e) {
    if (!silent) ElMessage.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

async function loadVersions(appID: number, force = false) {
  if (loadingVersions[appID]) return
  if (versions[appID] && !force) return
  loadingVersions[appID] = true
  try {
    const d = await api<{ versions: AppVersion[] }>('GET', `/api/apps/${appID}`)
    versions[appID] = d.versions || []
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    loadingVersions[appID] = false
  }
}

function onExpandChange(row: App, rows: App[]) {
  expanded.value = rows.map((r) => r.ID)
  if (rows.some((r) => r.ID === row.ID)) loadVersions(row.ID)
}

function toggleExpand(row: App) {
  tableRef.value?.toggleRowExpansion(row)
}

// ---- 勾选与批量删除 ----
const selection = ref<App[]>([])

function onSelectionChange(rows: App[]) {
  selection.value = rows
}

function clearSelection() {
  tableRef.value?.clearSelection()
}

async function batchDelete() {
  const n = selection.value.length
  try {
    await ElMessageBox.confirm(
      `删除选中的 ${n} 个应用？每个应用的全部版本与制品（tgz）都会被删除，不可恢复。`,
      '批量删除应用',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  const ids = selection.value.map((a) => a.ID)
  try {
    const r = await api<{ ok: number; failed: number }>('POST', '/api/apps/batch', { ids, action: 'delete' })
    if (r.failed === 0) ElMessage.success(`已删除 ${r.ok} 个应用`)
    else ElMessage.warning(`删除完成：${r.ok} 成功 / ${r.failed} 失败`)
    clearSelection()
    appSel.value = null
    await load()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

// ---- 上传新版本（展开区左上按钮：作用于当前展开的应用）----
const appSel = ref<App | null>(null)
const verVisible = ref(false)
const verLoading = ref(false)
const verForm = reactive({ note: '' })
const verFile = ref<File | null>(null)

function openAddVersion(app: App) {
  appSel.value = app
  Object.assign(verForm, { note: '' })
  verFile.value = null
  verVisible.value = true
}

function onVerFileChange(e: Event) {
  const input = e.target as HTMLInputElement
  verFile.value = input.files && input.files.length ? input.files[0] : null
}

async function submitAddVersion() {
  if (!appSel.value || !verFile.value) {
    ElMessage.warning('选择 chart.tgz 文件')
    return
  }
  verLoading.value = true
  try {
    await upload(`/api/apps/${appSel.value.ID}/versions/upload`, { note: verForm.note }, verFile.value)
    ElMessage.success('新版本已上传（版本号取自 chart.yaml，已置为最新）')
    verVisible.value = false
    await load()
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    verLoading.value = false
  }
}

// ---- 版本操作 ----
async function setLatest(app: App, v: AppVersion) {
  try {
    await api('POST', `/api/apps/${app.ID}/versions/${v.ID}/latest`, {})
    ElMessage.success(`已将 ${v.Version} 设为默认版本（latest）`)
    await load()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

async function deleteVersion(app: App, v: AppVersion) {
  try {
    await ElMessageBox.confirm(
      `删除 ${app.Name} 的版本 ${v.Version}？该版本 tgz 制品一并删除（不可恢复）。`,
      '删除版本', { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    // 版本列表不再消费 DELETE 响应：load() 会对展开行强制重拉版本
    //（loadVersions(id, true)），此处再赋值就是同一份数据拉两遍
    await api<AppVersion[]>('DELETE', `/api/apps/${app.ID}/versions/${v.ID}`)
    ElMessage.success('已删除')
    await load()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

function editFromVersion(app: App, v: AppVersion) {
  void router.push({ path: '/apps/ide', query: { app: String(app.ID), ...(v.Version ? { base: v.Version } : {}) } })
}

// 下载版本制品：attachment 响应直接触发浏览器下载（不走 api() —— 它
// 会把响应体按 JSON 解析）
function downloadVersion(app: App, v: AppVersion) {
  const a = document.createElement('a')
  a.href = appDownloadURL(app.ID, v.Version)
  a.download = ''
  document.body.appendChild(a)
  a.click()
  a.remove()
}

// ---- 版本作用域就地变更（不升版本——后期的权限/归属调整）----
const scopeVisible = ref(false)
const scopeLoading = ref(false)
const scopeSaving = ref(false)
const scopeApp = ref<App | null>(null)
const scopeVer = ref('')
const scopeForm = reactive({ pools: [] as string[], groups: [] as string[] })
const scopeLabels = ref<Record<string, string>>({})
const labelKeys = computed(() => labels.value.map((l) => l.Key))
const poolNames = computed(() => pools.value.map((p) => p.Name))
const groupNames = computed(() => groups.value.map((g) => g.Name))

// 打开即按该版本回填（spec 端点返回版本自己的 scope）；读取失败回退应用级
async function openScope(app: App, v: AppVersion) {
  scopeApp.value = app
  scopeVer.value = v.Version
  scopeForm.pools = []
  scopeForm.groups = []
  scopeLabels.value = parseLabels(app.Labels)
  scopeVisible.value = true
  scopeLoading.value = true
  try {
    const spec = await api<{ pools: string[] | null; groups: string[] | null; labels: string }>(
      'GET', `/api/apps/${app.ID}/spec?version=${encodeURIComponent(v.Version)}`,
    )
    scopeForm.pools = spec.pools || []
    scopeForm.groups = spec.groups || []
    scopeLabels.value = parseLabels(spec.labels)
  } catch (e) {
    // 读取失败必须中止并提示：静默回退应用级数据会让「保存」把版本作用域
    // 覆盖成应用级的
    scopeVisible.value = false
    ElMessage.error(`读取该版本的作用域失败：${(e as Error).message}（未打开，避免用应用级数据覆盖版本级配置）`)
  } finally {
    scopeLoading.value = false
  }
}

async function submitScope() {
  if (!scopeApp.value) return
  scopeSaving.value = true
  try {
    await api('PUT', `/api/apps/${scopeApp.value.ID}/scope`, {
      version: scopeVer.value,
      pools: scopeForm.pools,
      groups: scopeForm.groups,
      labels: JSON.stringify(scopeLabels.value || {}),
    })
    ElMessage.success(`已变更 ${scopeApp.value.Name}@${scopeVer.value} 的作用域（未升版本）`)
    scopeVisible.value = false
    await load()
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    scopeSaving.value = false
  }
}

onMounted(() => load())
</script>

<template>
  <div>
    <div class="toolbar">
      <span class="muted">版本号取自 chart.yaml 的 version；版本发布后不可覆盖（改号保存即新版本）；新建/上传入口在「新建应用」菜单</span>
      <div style="flex: 1" />
      <el-button :icon="Refresh" @click="load()">刷新</el-button>
    </div>

    <!-- 批量操作栏 -->
    <div v-if="selection.length" class="batch-bar">
      <span>已选 <b>{{ selection.length }}</b> 个应用</span>
      <el-button v-if="can('app:delete')" size="small" type="danger" plain :icon="Delete" @click="batchDelete">
        批量删除（含全部版本与制品）
      </el-button>
      <el-button size="small" link @click="clearSelection">取消选择</el-button>
    </div>

    <el-card shadow="never">
      <el-table
        ref="tableRef"
        :data="apps"
        v-loading="loading"
        row-key="ID"
        style="width: 100%"
        @selection-change="onSelectionChange"
        @expand-change="onExpandChange"
      >
        <el-table-column type="selection" width="44" />
        <el-table-column type="expand">
          <template #default="{ row }">
            <div class="versions">
              <div class="versions-head">
                <span class="muted">
                  共 {{ versions[row.ID]?.length ?? row.VersionCount }} 个版本 ·
                  默认版本 <b>{{ row.LatestVersion }}</b>（latest）
                </span>
                <div style="flex: 1" />
                <el-button v-if="can('app:upload')" size="small" :icon="Upload" @click="openAddVersion(row)">上传新版本</el-button>
              </div>
              <el-table :data="versions[row.ID] || []" size="small" v-loading="loadingVersions[row.ID]">
                <el-table-column label="版本" min-width="140">
                  <template #default="{ row: v }">
                    <el-tag v-if="v.Version === row.LatestVersion" type="success" size="small" effect="dark">latest</el-tag>
                    <span class="ver">{{ v.Version }}</span>
                  </template>
                </el-table-column>
                <el-table-column prop="Note" label="备注" min-width="160">
                  <template #default="{ row: v }"><span class="muted">{{ v.Note || '-' }}</span></template>
                </el-table-column>
                <el-table-column label="大小" width="90">
                  <template #default="{ row: v }">{{ (v.Size / 1024).toFixed(0) }} KB</template>
                </el-table-column>
                <el-table-column prop="CreatedAt" label="修改时间" min-width="165">
                  <template #default="{ row: v }"><span class="muted">{{ v.CreatedAt }}</span></template>
                </el-table-column>
                <el-table-column label="操作" width="360" fixed="right">
                  <template #default="{ row: v }">
                    <el-button v-if="can('app:edit')" link type="primary" :icon="EditPen" @click="editFromVersion(row, v)">修改</el-button>
                    <el-button link :icon="Download" @click="downloadVersion(row, v)">下载</el-button>
                    <el-button v-if="can('app:scope')" link :icon="PriceTag" @click="openScope(row, v)">作用域</el-button>
                    <el-button
                      v-if="v.Version !== row.LatestVersion"
                      link
                      type="primary"
                      :icon="Star"
                      @click="setLatest(row, v)"
                    >
                      设为默认
                    </el-button>
                    <el-button v-if="can('app:delete')" link type="danger" @click="deleteVersion(row, v)">删除</el-button>
                  </template>
                </el-table-column>
                <template #empty><el-empty description="该应用暂无版本" /></template>
              </el-table>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="应用" min-width="160">
          <template #default="{ row }">
            <el-button link type="primary" @click="toggleExpand(row)">{{ row.Name }}</el-button>
          </template>
        </el-table-column>
        <el-table-column label="默认版本" min-width="120">
          <template #default="{ row }">
            <el-tag type="success" effect="plain">{{ row.LatestVersion || '-' }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="版本数" width="80" prop="VersionCount" />
        <el-table-column label="池" min-width="120">
          <template #default="{ row }">
            <el-tag v-for="p in row.Pools || []" :key="p" type="warning" effect="plain" size="small" class="tag">{{ p }}</el-tag>
            <span v-if="!(row.Pools || []).length" class="muted">-</span>
          </template>
        </el-table-column>
        <el-table-column label="组" min-width="100">
          <template #default="{ row }">
            <el-tag v-for="g in row.Groups || []" :key="g" type="info" effect="plain" size="small" class="tag">{{ g }}</el-tag>
            <span v-if="!(row.Groups || []).length" class="muted">-</span>
          </template>
        </el-table-column>
        <el-table-column label="标签" min-width="140">
          <template #default="{ row }">
            <el-tag v-for="(v, k) in parseLabels(row.Labels)" :key="k" size="small" effect="plain" class="tag">
              {{ v ? `${k}=${v}` : k }}
            </el-tag>
            <span v-if="!Object.keys(parseLabels(row.Labels)).length" class="muted">-</span>
          </template>
        </el-table-column>
        <el-table-column prop="UpdatedAt" label="最近修改" min-width="170">
          <template #default="{ row }"><span class="muted">{{ row.UpdatedAt }}</span></template>
        </el-table-column>
        <template #empty>
          <el-empty description="还没有应用：上传 chart.tgz 创建，或在「新建应用」用 IDE 从零搭建" />
        </template>
      </el-table>
    </el-card>

    <!-- 版本作用域就地变更（不升版本） -->
    <el-dialog v-model="scopeVisible" :title="`作用域变更 · ${scopeApp?.Name || ''}@${scopeVer}`" width="560px">
      <el-alert type="info" :closable="false" show-icon style="margin-bottom: 14px"
        title="就地变更该版本的池/组/标签，不产生新版本（后期的权限调整）；默认版本会同步应用级作用域。" />
      <el-form label-width="90px" v-loading="scopeLoading">
        <el-form-item label="池（多选）">
          <el-select v-model="scopeForm.pools" multiple style="width: 100%">
            <el-option v-for="p in poolNames" :key="p" :label="p" :value="p" />
          </el-select>
        </el-form-item>
        <el-form-item label="组（多选）">
          <el-select v-model="scopeForm.groups" multiple style="width: 100%">
            <el-option v-for="g in groupNames" :key="g" :label="g" :value="g" />
          </el-select>
        </el-form-item>
        <el-form-item label="标签">
          <LabelRows v-model="scopeLabels" :keys="labelKeys" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="scopeVisible = false">取消</el-button>
        <el-button type="primary" :loading="scopeSaving" @click="submitScope">保存</el-button>
      </template>
    </el-dialog>

    <!-- 上传新版本 -->
    <el-dialog v-model="verVisible" :title="`上传新版本 · ${appSel?.Name || ''}`" width="480px">
      <el-alert type="info" :closable="false" show-icon style="margin-bottom: 12px"
        title="版本号取自 chart.yaml 的 version：与已有版本重复会被拒绝（版本不可覆盖），请先改 chart.yaml。" />
      <el-form label-width="90px">
        <el-form-item label="chart.tgz" required>
          <input type="file" accept=".tgz" @change="onVerFileChange" />
        </el-form-item>
        <el-form-item label="备注">
          <el-input v-model="verForm.note" placeholder="本次修改说明" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="verVisible = false">取消</el-button>
        <el-button type="primary" :loading="verLoading" @click="submitAddVersion">上传</el-button>
      </template>
    </el-dialog>
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
.versions {
  padding: 4px 10px 10px 48px;
}
.versions-head {
  display: flex;
  align-items: center;
  margin-bottom: 8px;
}
.ver {
  margin-left: 6px;
  font-family: ui-monospace, Menlo, monospace;
}
.tag {
  margin: 2px 4px 2px 0;
}
.muted {
  color: #909399;
  font-size: 12px;
}
</style>
