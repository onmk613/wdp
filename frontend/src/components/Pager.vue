<script setup lang="ts">
// 通用分页条：页大小选择、跳页、总数展示。与后端 pagedResp 信封对齐
//（items/total/page/page_size）。所有列表页共用——分页语义只在组件与
// usePaging 里实现一份，避免五个页面各自漂移。
import { computed } from 'vue'

const props = withDefaults(defineProps<{
  page: number
  pageSize: number
  total: number
  /** 可选页档（默认 [20, 50, 100, 200]；上限与后端 500 对齐） */
  sizes?: number[]
}>(), {
  sizes: () => [20, 50, 100, 200],
})

const emit = defineEmits<{
  (e: 'update:page', v: number): void
  (e: 'update:pageSize', v: number): void
}>()

const totalPages = computed(() => Math.max(1, Math.ceil(props.total / props.pageSize)))

function go(p: number) {
  if (p < 1 || p > totalPages.value || p === props.page) return
  emit('update:page', p)
}

function changeSize(v: number) {
  // 换页大小时尽量保持当前页的起始条目：先按比例换算页码，钳制到界内
  const firstItem = (props.page - 1) * props.pageSize
  emit('update:pageSize', v)
  emit('update:page', Math.floor(firstItem / v) + 1)
}

const from = computed(() => (props.total === 0 ? 0 : (props.page - 1) * props.pageSize + 1))
const to = computed(() => Math.min(props.page * props.pageSize, props.total))

</script>

<template>
  <div class="pager">
    <span class="muted total">{{ total }} 条 · 第 {{ from }}–{{ to }} 条</span>
    <el-pagination
      :current-page="page"
      :page-size="pageSize"
      :page-sizes="sizes"
      :total="total"
      layout="total, sizes, prev, pager, next, jumper"
      background
      @current-change="go"
      @size-change="changeSize"
    >
      <!-- windowPages 自绘页码仅作后备；el-pagination 自带收缩，默认用它的 -->
    </el-pagination>
  </div>
</template>

<style scoped>
.pager {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.total { white-space: nowrap; }
</style>
