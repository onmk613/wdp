<script setup lang="ts">
// 批量设置池/组/标签对话框（作用于勾选的主机），自 HostsPage 拆出。
// 勾选的项整体替换（多值，空=清除）；未勾选保持不变。
import { reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { api, type BatchResponse } from '../../api'
import LabelRows from '../LabelRows.vue'

const props = defineProps<{
  ids: number[] // 勾选中的主机
  poolNames: string[]
  groupNames: string[]
  labelKeys: string[]
}>()
const visible = defineModel<boolean>({ required: true })
const emit = defineEmits<{ (e: 'applied'): void }>()

const form = reactive({ pools: [] as string[], groups: [] as string[], replace: false })
const labels = ref<Record<string, string>>({})
const setPools = ref(false)
const setGroups = ref(false)
const loading = ref(false)

watch(visible, (v) => {
  if (!v) return
  Object.assign(form, { pools: [], groups: [], replace: false })
  labels.value = {}
  setPools.value = false
  setGroups.value = false
})

async function submit() {
  const body: Record<string, unknown> = {
    ids: props.ids,
    action: 'assign',
    labels: labels.value,
    replace_labels: form.replace,
  }
  if (setPools.value) {
    body.pools = form.pools
    body.set_pools = true
  }
  if (setGroups.value) {
    body.groups = form.groups
    body.set_groups = true
  }
  loading.value = true
  try {
    const r = await api<BatchResponse>('POST', '/api/hosts/batch', body)
    ElMessage.success(`已更新 ${r.ok} 台`)
    visible.value = false
    emit('applied')
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <el-dialog v-model="visible" title="批量设置池 / 组 / 标签" width="520px">
    <el-alert type="info" :closable="false" show-icon style="margin-bottom: 16px"
      :title="`将应用于选中的 ${ids.length} 台主机；未改动的项保持不变`" />
    <el-form label-width="90px">
      <el-form-item label="设置池">
        <el-checkbox v-model="setPools">修改池归属（整体替换，空 = 清除）</el-checkbox>
        <el-select v-model="form.pools" multiple :disabled="!setPools" placeholder="替换为这些池" style="width: 100%">
          <el-option v-for="p in poolNames" :key="p" :label="p" :value="p" />
        </el-select>
      </el-form-item>
      <el-form-item label="设置组">
        <el-checkbox v-model="setGroups">修改组归属（整体替换，空 = 清除）</el-checkbox>
        <el-select v-model="form.groups" multiple :disabled="!setGroups" placeholder="替换为这些组" style="width: 100%">
          <el-option v-for="g in groupNames" :key="g" :label="g" :value="g" />
        </el-select>
      </el-form-item>
      <el-form-item label="标签">
        <LabelRows v-model="labels" :keys="labelKeys" />
        <el-checkbox v-model="form.replace" style="margin-top: 6px">替换全部标签（而非追加）</el-checkbox>
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="loading" @click="submit">应用</el-button>
    </template>
  </el-dialog>
</template>
