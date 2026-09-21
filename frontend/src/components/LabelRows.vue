<script setup lang="ts">
// 标签键值行编辑器：键下拉可选已有标签键（注册表），也可输入新键；
// 值可留空（纯键标签）。主机编辑 / 批量设置 / 版本作用域共用。
import { reactive, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { Delete, Plus } from '@element-plus/icons-vue'

const props = defineProps<{ modelValue: Record<string, string>; keys: string[] }>()
const emit = defineEmits<{ (e: 'update:modelValue', v: Record<string, string>): void }>()

type Row = { key: string; value: string }
const rows = reactive<Row[]>([])

// 当前行内容对应的对象（与 sync 的产出同构，用于回环判定）
function currentObj(): Record<string, string> {
  const out: Record<string, string> = {}
  for (const r of rows) {
    const k = r.key.trim()
    if (k) out[k] = r.value.trim()
  }
  return out
}

function rebuild(obj: Record<string, string>) {
  rows.length = 0
  for (const [k, v] of Object.entries(obj || {})) rows.push({ key: k, value: v || '' })
  if (!rows.length) rows.push({ key: '', value: '' })
}

// 键重复时拒绝提交：currentObj 后写覆盖先写会静默丢一行；同一重复键只警
// 告一次（watch 每击键都进 sync），改回唯一后恢复提交
let warnedDup = ''

function sync() {
  const seen = new Set<string>()
  for (const r of rows) {
    const k = r.key.trim()
    if (!k) continue
    if (seen.has(k)) {
      if (warnedDup !== k) {
        warnedDup = k
        ElMessage.warning(`标签键 "${k}" 在多行重复：键必须唯一，请修改后才会提交`)
      }
      return // 不写入，父级保持上一份有效值
    }
    seen.add(k)
  }
  warnedDup = ''
  emit('update:modelValue', currentObj())
}

// 外部值变化才重建行；与本组件刚发回的值一致时跳过——emit 每次都是新
// 对象引用，不比对内容会 rebuild → 行 watch → 再 emit 死循环（下拉被
// 持续重渲染，无法点击）。空行（rows.length=0）必须重建：初始 {} 与空
// 行内容相等，不能因此跳过初始化。
watch(() => props.modelValue, (v) => {
  if (rows.length && JSON.stringify(v || {}) === JSON.stringify(currentObj())) return
  rebuild(v)
}, { immediate: true, deep: true })
watch(rows, sync, { deep: true })

function addRow() {
  rows.push({ key: '', value: '' })
}
function removeRow(i: number) {
  rows.splice(i, 1)
  if (!rows.length) rows.push({ key: '', value: '' })
}

// 键下拉选项：已有键里去掉本表单已占用的（避免同键重复相互覆盖）
function keyOptions(usedBy: Row): string[] {
  const used = new Set(rows.filter((r) => r !== usedBy).map((r) => r.key.trim()))
  return props.keys.filter((k) => !used.has(k))
}
</script>

<template>
  <div class="label-rows">
    <div v-for="(row, i) in rows" :key="i" class="label-row">
      <el-select :model-value="row.key" filterable allow-create default-first-option
        placeholder="选择或输入标签键" size="small" class="key-sel"
        @update:model-value="row.key = String($event || '')">
        <el-option v-for="k in keyOptions(row)" :key="k" :label="k" :value="k" />
      </el-select>
      <span class="eq">=</span>
      <el-input v-model="row.value" placeholder="值（可空）" size="small" class="val-input" />
      <el-button link type="danger" :icon="Delete" size="small" @click="removeRow(i)" />
    </div>
    <el-button size="small" :icon="Plus" @click="addRow">添加标签</el-button>
  </div>
</template>

<style scoped>
.label-rows {
  width: 100%;
}
.label-row {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-bottom: 6px;
}
.key-sel {
  width: 55%;
}
.eq {
  color: #909399;
}
.val-input {
  flex: 1;
}
</style>
