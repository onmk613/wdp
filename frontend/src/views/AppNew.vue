<script setup lang="ts">
// 新建应用入口（含草稿箱）：两个创建入口（IDE 新建 / 上传 chart 包）+
// 草稿箱（暂存与中断的编辑草稿，继续编辑/删除）。弹窗不自动打开——
// 手动点击「IDE 新建」卡片才收集名称/版本/描述。
import { onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Edit, Refresh, Upload } from '@element-plus/icons-vue'
import { upload, listAppDrafts, deleteAppDraft, type App, type AppDraftItem } from '../api'

const router = useRouter()
const dialogVisible = ref(false)

const appNameRe = /^[a-zA-Z0-9][a-zA-Z0-9._-]*$/
const form = reactive({ name: '', version: '1.0.0', description: '' })

function nameErr(): string {
  const n = form.name.trim()
  if (!n) return '应用名必填'
  if (!appNameRe.test(n)) return '字母数字开头，可用 . _ -'
  return ''
}

function goIDE() {
  if (nameErr()) return
  dialogVisible.value = false
  void router.push({
    path: '/apps/ide',
    query: {
      new: '1',
      name: form.name.trim(),
      version: form.version.trim() || '1.0.0',
      desc: form.description.trim(),
    },
  })
}

// ---- 上传 chart 包 ----
const uploading = ref(false)
const picker = ref<HTMLInputElement | null>(null)

async function onChartPicked(e: Event) {
  const input = e.target as HTMLInputElement
  const file = input.files && input.files.length ? input.files[0] : null
  input.value = ''
  if (!file) return
  uploading.value = true
  try {
    const app = await upload<App>('/api/apps/upload', {}, file)
    ElMessage.success(`应用 ${app.Name}@${app.LatestVersion} 已入库`)
    void router.push({ path: '/apps/ide', query: { app: String(app.ID) } })
  } catch (err) {
    ElMessage.error((err as Error).message)
  } finally {
    uploading.value = false
  }
}

// ---- 草稿箱（与创建入口同页：改稿的第一站是这里，不是菜单深处）----
const draftsLoading = ref(false)
const drafts = ref<AppDraftItem[]>([])

async function refreshDrafts() {
  draftsLoading.value = true
  try {
    drafts.value = await listAppDrafts()
  } catch (e) {
    ElMessage.error(`加载草稿失败：${(e as Error).message}`)
  } finally {
    draftsLoading.value = false
  }
}
onMounted(refreshDrafts)

// 继续编辑：直达 IDE 并自动恢复该草稿（?restore=1，跳过确认横幅）
function continueEdit(d: AppDraftItem) {
  if (d.kind === 'new') {
    void router.push({ path: '/apps/ide', query: { new: '1', name: d.app_name, restore: '1' } })
  } else {
    void router.push({ path: '/apps/ide', query: { app: d.key, restore: '1' } })
  }
}

async function removeDraft(d: AppDraftItem) {
  const label = d.kind === 'new' ? `新建草稿 ${d.app_name}` : `应用 ${d.app_name} 的编辑草稿`
  try {
    await ElMessageBox.confirm(`删除${label}？（不可恢复）`, '删除草稿', {
      confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning',
    })
  } catch { return }
  try {
    await deleteAppDraft(d.key)
    ElMessage.success('草稿已删除')
    await refreshDrafts()
  } catch (e) {
    ElMessage.error(`删除失败：${(e as Error).message}`)
  }
}

function fmtTime(iso: string): string {
  return iso ? new Date(iso).toLocaleString('zh-CN', { hour12: false }) : '-'
}
</script>

<template>
  <div class="appnew">
    <el-row :gutter="16">
      <el-col :span="12">
        <el-card shadow="never" class="entry" @click="dialogVisible = true">
          <el-icon :size="28" color="#2563eb"><Edit /></el-icon>
          <div class="entry-title">IDE 新建</div>
          <p class="muted">输入名称/版本/描述进入编辑器：目录树 + 多标签文本编辑，
            模块/参数/模板变量全程自动补全，校验通过后保存入库。</p>
        </el-card>
      </el-col>
      <el-col :span="12">
        <el-card shadow="never" class="entry" @click="picker?.click()">
          <el-icon :size="28" color="#059669"><Upload /></el-icon>
          <div class="entry-title">上传 chart 包（.tgz）</div>
          <p class="muted">已有离线构建的 chart 包直接上传（chart.yaml 的 name/version
            定应用与版本）；上传后可在 IDE 里继续修改。</p>
        </el-card>
      </el-col>
    </el-row>

    <!-- 草稿箱 -->
    <el-card shadow="never" class="drafts" v-loading="draftsLoading">
      <template #header>
        <div class="drafts-head">
          <b>草稿箱</b>
          <span class="muted">暂存与中断留下的编辑草稿 · 按用户隔离 · 继续编辑自动恢复</span>
          <div style="flex: 1" />
          <el-button size="small" :icon="Refresh" @click="refreshDrafts">刷新</el-button>
        </div>
      </template>
      <el-table :data="drafts" size="small" empty-text="暂无草稿：在应用编辑器里暂存，或编辑中断后自动出现">
        <el-table-column label="类型" width="90">
          <template #default="{ row }">
            <el-tag size="small" :type="row.kind === 'new' ? 'warning' : 'info'" effect="plain">
              {{ row.kind === 'new' ? '新建中' : '编辑中' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="应用" min-width="180">
          <template #default="{ row }">
            <span :class="{ gone: row.app_gone }">{{ row.app_name }}</span>
            <el-tag v-if="row.app_gone" size="small" type="danger" effect="plain" style="margin-left: 6px">
              应用已删除
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="编辑底本" width="110">
          <template #default="{ row }">
            <span class="muted">{{ row.base_version || '（新建）' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="暂存时间" width="180">
          <template #default="{ row }">{{ fmtTime(row.updated_at) }}</template>
        </el-table-column>
        <el-table-column label="操作" width="170" fixed="right">
          <template #default="{ row }">
            <el-button size="small" type="primary" text :disabled="row.app_gone" @click="continueEdit(row)">
              继续编辑
            </el-button>
            <el-button size="small" type="danger" text @click="removeDraft(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-dialog v-model="dialogVisible" title="新建应用" width="480px" :close-on-click-modal="false">
      <el-form label-width="80px" @submit.prevent>
        <el-form-item label="应用名" required>
          <el-input v-model="form.name" placeholder="字母数字开头，可用 . _ -（作为 chart 名与默认主机组）" />
          <span v-if="nameErr()" class="err">{{ nameErr() }}</span>
        </el-form-item>
        <el-form-item label="版本">
          <el-input v-model="form.version" placeholder="默认 1.0.0（写入 chart.yaml）" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="form.description" type="textarea" :rows="2" placeholder="可选，写入 chart.yaml" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :disabled="!!nameErr()" @click="goIDE">确定，进入编辑器</el-button>
      </template>
    </el-dialog>

    <input ref="picker" type="file" accept=".tgz" hidden @change="onChartPicked" />
    <div v-if="uploading" class="muted up">上传中…</div>
  </div>
</template>

<style scoped>
.entry {
  cursor: pointer;
  transition: border-color 0.15s;
  height: 100%;
}
.entry:hover { border-color: #2563eb; }
.entry-title {
  font-weight: 600;
  margin: 10px 0 6px;
  font-size: 15px;
}
.drafts { margin-top: 16px; }
.drafts-head {
  display: flex;
  align-items: center;
  gap: 10px;
}
.muted { color: #909399; font-size: 12px; }
.err { color: #dc2626; font-size: 12px; }
.up { margin-top: 10px; text-align: center; }
.gone { text-decoration: line-through; color: #c0c4cc; }
</style>
