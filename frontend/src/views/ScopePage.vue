<script setup lang="ts">
// 池 / 组 / 标签 管理页（同一组件按 kind 实例化三次）：
// 注册表 CRUD + 成员视图。成员 = 命中该分类的主机与应用（主机按池/组/
// 标签键匹配，应用按应用级 scope 匹配），点击主机跳主机管理定位、点击
// 应用直接进编辑器。台账里被引用但未注册的分类名（如 CSV 导入带入的新
// 名）也列出，标记「未注册」。
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Delete, Plus, Refresh } from '@element-plus/icons-vue'
import { api, type App, type GroupEntry, type Host, type LabelDef, type Pool } from '../api'

const props = defineProps<{ kind: 'pool' | 'group' | 'label' }>()
const router = useRouter()

const kindMeta = {
  pool: { label: '池', labelPlural: '池', nameKey: 'Name', createBody: (name: string, note: string) => ({ name, note }) },
  group: { label: '组', labelPlural: '组', nameKey: 'Name', createBody: (name: string, note: string) => ({ name, note }) },
  label: { label: '标签', labelPlural: '标签', nameKey: 'Key', createBody: (name: string, note: string) => ({ key: name, note }) },
} as const
const meta = computed(() => kindMeta[props.kind])

const hosts = ref<Host[]>([])
const apps = ref<App[]>([])
const pools = ref<Pool[]>([])
const groups = ref<GroupEntry[]>([])
const labelDefs = ref<LabelDef[]>([])
const loading = ref(false)

interface Entry {
  id: number // 0 = 未注册（仅台账引用）
  name: string
  note: string
  hosts: { name: string; value: string }[]
  apps: App[]
}

function labelsOf(raw: string): Record<string, string> {
  try {
    return JSON.parse(raw || '{}')
  } catch {
    return {}
  }
}

function memberOf(kind: 'pool' | 'group' | 'label', name: string, list: string[] | null, labels: string): boolean {
  if (kind === 'label') return Object.prototype.hasOwnProperty.call(labelsOf(labels), name)
  return (list || []).includes(name)
}

// 注册表 + 未注册引用名的并集，附成员明细
const entries = computed<Entry[]>(() => {
  const reg: { id: number; name: string; note: string }[] =
    props.kind === 'pool'
      ? pools.value.map((p) => ({ id: p.ID, name: p.Name, note: p.Note }))
      : props.kind === 'group'
        ? groups.value.map((g) => ({ id: g.ID, name: g.Name, note: g.Note }))
        : labelDefs.value.map((l) => ({ id: l.ID, name: l.Key, note: l.Note }))
  const used = new Set<string>()
  for (const h of hosts.value) {
    if (props.kind === 'label') {
      Object.keys(labelsOf(h.Labels)).forEach((k) => used.add(k))
    } else {
      ;((props.kind === 'pool' ? h.Pools : h.Groups) || []).forEach((n) => used.add(n))
    }
  }
  for (const a of apps.value) {
    if (props.kind === 'label') {
      Object.keys(labelsOf(a.Labels)).forEach((k) => used.add(k))
    } else {
      ;((props.kind === 'pool' ? a.Pools : a.Groups) || []).forEach((n) => used.add(n))
    }
  }
  const all = [...reg]
  for (const n of used) {
    if (!reg.some((r) => r.name === n)) all.push({ id: 0, name: n, note: '' })
  }
  all.sort((x, y) => (x.id === 0 ? 1 : 0) - (y.id === 0 ? 1 : 0) || x.name.localeCompare(y.name))
  return all.map((r) => ({
    ...r,
    hosts: hosts.value
      .filter((h) => memberOf(props.kind, r.name, props.kind === 'pool' ? h.Pools : h.Groups, h.Labels))
      .map((h) => ({ name: h.Name, value: labelsOf(h.Labels)[r.name] ?? '' })),
    apps: apps.value.filter((a) => memberOf(props.kind, r.name, props.kind === 'pool' ? a.Pools : a.Groups, a.Labels)),
  }))
})

async function load() {
  loading.value = true
  try {
    ;[hosts.value, apps.value, pools.value, groups.value, labelDefs.value] = await Promise.all([
      api<Host[]>('GET', '/api/hosts'),
      api<App[]>('GET', '/api/apps'),
      api<Pool[]>('GET', '/api/pools'),
      api<GroupEntry[]>('GET', '/api/groups'),
      api<LabelDef[]>('GET', '/api/labels'),
    ])
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
onMounted(load)

// ---- 新建 ----
const createVisible = ref(false)
const createLoading = ref(false)
const createForm = reactive({ name: '', note: '' })

function openCreate() {
  Object.assign(createForm, { name: '', note: '' })
  createVisible.value = true
}

async function submitCreate() {
  const name = createForm.name.trim()
  if (!name) {
    ElMessage.warning(`${meta.value.label}名必填`)
    return
  }
  createLoading.value = true
  try {
    await api('POST', `/api/${props.kind}s`, meta.value.createBody(name, createForm.note))
    ElMessage.success(`已建${meta.value.label} ${name}`)
    createVisible.value = false
    load()
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    createLoading.value = false
  }
}

async function removeEntry(entry: Entry) {
  const who = entry.hosts.length + entry.apps.length
  try {
    await ElMessageBox.confirm(
      who > 0
        ? `${meta.value.label} ${entry.name} 下有 ${entry.hosts.length} 台主机、${entry.apps.length} 个应用：删除仅解除归属，不删除成员。`
        : `删除${meta.value.label} ${entry.name}？`,
      `删除${meta.value.label}`,
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await api('DELETE', `/api/${props.kind}s/${entry.id}`)
    ElMessage.success(`已删除${meta.value.label} ${entry.name}`)
    load()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

// ---- 跳转 ----
function gotoHost(name: string) {
  void router.push({ path: '/hosts', query: { search: name } })
}
function gotoApp(app: App) {
  void router.push(`/apps/${app.ID}/edit`)
}
</script>

<template>
  <div>
    <div class="toolbar">
      <span class="muted">
        按分类查看成员：点击主机跳转主机管理并定位，点击应用直接进入编辑器；应用级作用域取最近保存的版本
      </span>
      <div style="flex: 1" />
      <el-button :icon="Refresh" @click="load()">刷新</el-button>
      <el-button type="primary" :icon="Plus" @click="openCreate">新建{{ meta.label }}</el-button>
    </div>

    <el-card shadow="never">
      <el-table :data="entries" v-loading="loading" row-key="name" style="width: 100%">
        <el-table-column type="expand">
          <template #default="{ row }">
            <div class="members">
              <div class="members-row">
                <span class="members-label">主机（{{ row.hosts.length }}）</span>
                <template v-if="row.hosts.length">
                  <el-tag
                    v-for="h in row.hosts" :key="h.name" class="member-chip" effect="plain"
                    @click="gotoHost(h.name)"
                  >{{ h.name }}<span v-if="h.value" class="member-value">={{ h.value }}</span></el-tag>
                </template>
                <span v-else class="muted">无</span>
              </div>
              <div class="members-row">
                <span class="members-label">应用（{{ row.apps.length }}）</span>
                <template v-if="row.apps.length">
                  <el-tag
                    v-for="a in row.apps" :key="a.ID" class="member-chip" type="warning" effect="plain"
                    @click="gotoApp(a)"
                  >{{ a.Name }}@{{ a.LatestVersion }}</el-tag>
                </template>
                <span v-else class="muted">无</span>
              </div>
            </div>
          </template>
        </el-table-column>
        <el-table-column :label="meta.label + '名'" min-width="160">
          <template #default="{ row }">
            <b>{{ row.name }}</b>
            <el-tag v-if="!row.id" size="small" type="info" style="margin-left: 6px">未注册</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="备注" min-width="180">
          <template #default="{ row }">
            <span v-if="row.note">{{ row.note }}</span>
            <span v-else class="muted">—</span>
          </template>
        </el-table-column>
        <el-table-column label="主机" width="90">
          <template #default="{ row }">{{ row.hosts.length }}</template>
        </el-table-column>
        <el-table-column label="应用" width="90">
          <template #default="{ row }">{{ row.apps.length }}</template>
        </el-table-column>
        <el-table-column label="操作" width="90">
          <template #default="{ row }">
            <el-button v-if="row.id" size="small" type="danger" plain :icon="Delete" @click="removeEntry(row)">删除</el-button>
            <span v-else class="muted" title="仅台账引用，去成员处编辑可移除">—</span>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-dialog v-model="createVisible" :title="`新建${meta.label}`" width="440px">
      <el-form label-width="80px">
        <el-form-item :label="meta.label + '名'" required>
          <el-input v-model="createForm.name" placeholder="唯一标识" />
        </el-form-item>
        <el-form-item label="备注">
          <el-input v-model="createForm.note" placeholder="可选" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="createVisible = false">取消</el-button>
        <el-button type="primary" :loading="createLoading" @click="submitCreate">创建</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.toolbar {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
}
.muted {
  color: #909399;
  font-size: 12px;
}
.members {
  padding: 4px 12px 8px;
}
.members-row {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  padding: 4px 0;
}
.members-label {
  color: #909399;
  font-size: 13px;
  min-width: 72px;
}
.member-chip {
  cursor: pointer;
}
.member-chip:hover {
  opacity: 0.75;
}
.member-value {
  opacity: 0.7;
  margin-left: 2px;
}
</style>
