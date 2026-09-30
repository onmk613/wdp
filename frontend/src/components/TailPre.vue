<script setup lang="ts">
// 长输出分级展示：默认只渲染尾部 N 行（对齐 CLI 任务级 output: tail=N
// 语义），「展开全部」切换全量。stdout 全量落库但 UI 一次渲染几百台 ×
// 上千行 <pre> 会直接卡死页面——默认尾部即所见即所查（排查看结尾）。
import { computed, ref } from 'vue'

const props = withDefaults(defineProps<{ text: string; lines?: number }>(), { lines: 50 })

const all = computed(() => (props.text || '').split('\n'))
const over = computed(() => all.value.length > props.lines)
const hiddenCount = computed(() => Math.max(0, all.value.length - props.lines))
const tail = computed(() => all.value.slice(-props.lines).join('\n'))
const expanded = ref(false)
</script>

<template>
  <div class="tailpre">
    <div v-if="over" class="tailpre-note">
      <span v-if="!expanded" class="muted">已折叠前 {{ hiddenCount }} 行（共 {{ all.length }} 行）</span>
      <el-button link type="primary" size="small" @click="expanded = !expanded">
        {{ expanded ? `收起为尾部 ${props.lines} 行` : '展开全部' }}
      </el-button>
    </div>
    <pre class="out">{{ over && !expanded ? tail : text }}</pre>
  </div>
</template>

<style scoped>
.tailpre-note {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 6px 0 2px;
  font-size: 12px;
}
.muted { color: #909399; }
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
  margin: 0;
}
</style>
