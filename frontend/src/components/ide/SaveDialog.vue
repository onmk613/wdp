<script setup lang="ts">
// 保存对话框：版本号（默认取 chart.yaml 内版本——未被占用时；否则自动
// 推进到下一个可用号）、描述、作用域（池/组/标签）。确认后由 AppIDE
// 负责把版本号同步写进 chart.yaml（保持文件与库一致）再走校验+保存。
import { reactive, watch } from 'vue'
import type { Pool, GroupEntry } from '../../api'

const props = defineProps<{
  visible: boolean
  isCreate: boolean
  suggestedVersion: string
  defaultDescription: string
  takenVersions: string[]
  pools: Pool[]
  groups: GroupEntry[]
  current: { pools: string[]; groups: string[]; labels: string }
}>()
const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
  (e: 'confirm', v: { version: string; description: string; pools: string[]; groups: string[]; labels: string }): void
}>()

const form = reactive({ version: '', description: '', pools: [] as string[], groups: [] as string[], labels: '{}' })

watch(
  () => props.visible,
  (v) => {
    if (!v) return
    form.version = props.suggestedVersion
    form.description = props.defaultDescription
    form.pools = [...props.current.pools]
    form.groups = [...props.current.groups]
    form.labels = props.current.labels || '{}'
  },
)

const versionRe = /^[a-zA-Z0-9][a-zA-Z0-9._-]*$/
function versionErr(): string {
  const v = form.version.trim()
  if (!v) return '版本号必填'
  if (!versionRe.test(v)) return '字母数字开头，可用 . _ -'
  if (!props.isCreate && props.takenVersions.includes(v)) return `版本 ${v} 已存在（发布后不可覆盖）`
  return ''
}
function labelsErr(): string {
  try {
    const o = JSON.parse(form.labels || '{}')
    if (o && typeof o === 'object' && !Array.isArray(o)) return ''
  } catch { /* fallthrough */ }
  return '标签必须是 JSON 对象'
}

function ok() {
  if (versionErr() || labelsErr()) return
  emit('update:visible', false)
  emit('confirm', {
    version: form.version.trim(),
    description: form.description,
    pools: form.pools,
    groups: form.groups,
    labels: form.labels.trim() || '{}',
  })
}
</script>

<template>
  <el-dialog
    :model-value="visible"
    :title="isCreate ? '保存（创建应用）' : '保存为新版本'"
    width="560px"
    :close-on-click-modal="false"
    @update:model-value="emit('update:visible', $event)"
  >
    <el-form label-width="88px">
      <el-form-item label="版本号" required>
        <el-input v-model="form.version" placeholder="写入 chart.yaml 的 version" />
        <span v-if="versionErr()" class="err">{{ versionErr() }}</span>
        <span v-else class="hint">确认后自动同步写入 chart.yaml（文件与库保持一致）</span>
      </el-form-item>
      <el-form-item label="描述">
        <el-input v-model="form.description" placeholder="同步写入 chart.yaml 的 description" />
      </el-form-item>
      <el-form-item label="池（多选）">
        <el-select v-model="form.pools" multiple style="width: 100%">
          <el-option v-for="p in pools" :key="p.ID" :label="p.Name" :value="p.Name" />
        </el-select>
      </el-form-item>
      <el-form-item label="组（多选）">
        <el-select v-model="form.groups" multiple style="width: 100%">
          <el-option v-for="g in groups" :key="g.ID" :label="g.Name" :value="g.Name" />
        </el-select>
      </el-form-item>
      <el-form-item label="标签">
        <el-input v-model="form.labels" placeholder='JSON，如 {"env":"prod"}' />
        <span v-if="labelsErr()" class="err">{{ labelsErr() }}</span>
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="emit('update:visible', false)">取消</el-button>
      <el-button type="primary" :disabled="!!versionErr() || !!labelsErr()" @click="ok">校验并保存</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.err { color: #dc2626; font-size: 12px; }
.hint { color: #909399; font-size: 12px; }
</style>
