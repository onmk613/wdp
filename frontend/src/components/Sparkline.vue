<script setup lang="ts">
// 轻量 SVG 趋势图（不引图表库）：横轴时间刻度、纵轴值刻度、渐变面积、
// 可选 max 虚线；鼠标悬停吸附最近数据点，十字线 + 数值气泡。
// 数据为 5 分钟聚合桶 [{ts, avg, max}]。
import { computed, ref } from 'vue'

const props = withDefaults(
  defineProps<{
    points: { ts: number; avg: number; max: number }[]
    width?: number
    height?: number
    unit?: string // 值后缀（% / B/s …）
    color?: string
    showMax?: boolean // max 序列画虚线（峰值线）
  }>(),
  { width: 520, height: 150, unit: '', color: '#2563eb', showMax: false },
)

const PAD = { left: 48, right: 16, top: 12, bottom: 22 }

const view = computed(() => {
  const pts = props.points
  if (!pts.length) return null
  const t0 = pts[0].ts
  const t1 = Math.max(pts[pts.length - 1].ts, t0 + 1)
  let min = Infinity
  let max = -Infinity
  for (const p of pts) {
    min = Math.min(min, p.avg)
    max = Math.max(max, props.showMax ? Math.max(p.avg, p.max) : p.avg)
  }
  if (min === max) {
    min -= 1
    max += 1
  }
  const pad = (max - min) * 0.1
  min -= pad
  max += pad
  const iw = props.width - PAD.left - PAD.right
  const ih = props.height - PAD.top - PAD.bottom
  return {
    t0, t1, min, max, iw, ih,
    x: (ts: number) => PAD.left + ((ts - t0) / (t1 - t0)) * iw,
    y: (v: number) => PAD.top + ih - ((v - min) / (max - min)) * ih,
  }
})

const path = computed(() => {
  const v = view.value
  if (!v) return ''
  return props.points.map((p, i) => `${i === 0 ? 'M' : 'L'}${v.x(p.ts).toFixed(1)},${v.y(p.avg).toFixed(1)}`).join(' ')
})

const maxPath = computed(() => {
  const v = view.value
  if (!v || !props.showMax) return ''
  return props.points.map((p, i) => `${i === 0 ? 'M' : 'L'}${v.x(p.ts).toFixed(1)},${v.y(p.max).toFixed(1)}`).join(' ')
})

const area = computed(() => (path.value ? `${path.value} L${view.value!.x(view.value!.t1)},${props.height - PAD.bottom} L${view.value!.x(view.value!.t0)},${props.height - PAD.bottom} Z` : ''))

// 纵轴刻度：nice step（1/2/2.5/5 × 10^n）
const yTicks = computed(() => {
  const v = view.value
  if (!v) return []
  const raw = (v.max - v.min) / 4
  const exp = Math.floor(Math.log10(raw))
  const base = Math.pow(10, exp)
  const step = [1, 2, 2.5, 5, 10].find((m) => raw <= m * base) ? [1, 2, 2.5, 5, 10].find((m) => raw <= m * base)! * base : 10 * base
  const out: number[] = []
  for (let val = Math.ceil(v.min / step) * step; val <= v.max + step * 0.01; val += step) {
    out.push(val)
  }
  return out
})

// 横轴刻度：按跨度选时间步长（5 刻度以内）
const xTicks = computed(() => {
  const v = view.value
  if (!v) return []
  const span = v.t1 - v.t0
  const steps = [60, 300, 900, 1800, 3600, 3 * 3600, 6 * 3600, 12 * 3600, 86400, 2 * 86400, 7 * 86400, 14 * 86400, 30 * 86400]
  const step = steps.find((s) => span / s <= 5) ?? 30 * 86400
  const out: number[] = []
  for (let ts = Math.ceil(v.t0 / step) * step; ts <= v.t1; ts += step) {
    out.push(ts)
  }
  return out
})

const p2 = (n: number) => String(n).padStart(2, '0')

function fmtXTick(ts: number): string {
  const d = new Date(ts * 1000)
  const span = (view.value?.t1 ?? 0) - (view.value?.t0 ?? 0)
  if (span <= 24 * 3600) return `${p2(d.getHours())}:${p2(d.getMinutes())}`
  if (span <= 14 * 86400) return `${p2(d.getMonth() + 1)}-${p2(d.getDate())} ${p2(d.getHours())}时`
  return `${d.getMonth() + 1}-${p2(d.getDate())}`
}

function fmtFull(ts: number): string {
  const d = new Date(ts * 1000)
  return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())} ${p2(d.getHours())}:${p2(d.getMinutes())}`
}

function fmt(v: number): string {
  const a = Math.abs(v)
  if (a >= 1e9) return (v / 1e9).toFixed(2) + 'G'
  if (a >= 1e6) return (v / 1e6).toFixed(2) + 'M'
  if (a >= 1e3) return (v / 1e3).toFixed(1) + 'K'
  return v.toFixed(a < 10 ? 2 : 1)
}

// ---- 悬停：吸附最近数据点 ----
const hover = ref<{ x: number; y: number; ts: number; avg: number; max: number } | null>(null)

function onMove(e: MouseEvent) {
  const v = view.value
  if (!v) return
  const svg = e.currentTarget as SVGSVGElement
  const rect = svg.getBoundingClientRect()
  // SVG 可能被容器缩放，换算回 viewBox 坐标
  const px = (e.clientX - rect.left) * (props.width / rect.width)
  const pts = props.points
  let best = 0
  let bd = Infinity
  for (let i = 0; i < pts.length; i++) {
    const d = Math.abs(v.x(pts[i].ts) - px)
    if (d < bd) {
      bd = d
      best = i
    }
  }
  const p = pts[best]
  hover.value = { x: v.x(p.ts), y: v.y(p.avg), ts: p.ts, avg: p.avg, max: p.max }
}

const tipLeft = computed(() => {
  if (!hover.value) return 0
  return Math.min(Math.max(hover.value.x - 80, 4), props.width - 170)
})
</script>

<template>
  <div v-if="view" class="trend" :style="{ width: width + 'px' }">
    <svg :width="width" :height="height" :viewBox="`0 0 ${width} ${height}`" @mousemove="onMove" @mouseleave="hover = null">
      <defs>
        <linearGradient :id="`g-${color.replace('#', '')}`" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" :stop-color="color" stop-opacity="0.22" />
          <stop offset="100%" :stop-color="color" stop-opacity="0.02" />
        </linearGradient>
      </defs>
      <!-- 纵轴网格 + 刻度值 -->
      <g v-for="t in yTicks" :key="'y' + t">
        <line :x1="PAD.left" :y1="view.y(t)" :x2="width - PAD.right" :y2="view.y(t)" stroke="#eef1f6" stroke-width="1" />
        <text :x="PAD.left - 6" :y="view.y(t) + 3" text-anchor="end" class="tick">{{ fmt(t) }}</text>
      </g>
      <!-- 横轴刻度 -->
      <g v-for="t in xTicks" :key="'x' + t">
        <line :x1="view.x(t)" :y1="height - PAD.bottom" :x2="view.x(t)" :y2="height - PAD.bottom + 4" stroke="#c0c4cc" stroke-width="1" />
        <text :x="view.x(t)" :y="height - PAD.bottom + 16" text-anchor="middle" class="tick">{{ fmtXTick(t) }}</text>
      </g>
      <!-- 面积 + 均值线 + 峰值虚线 -->
      <path :d="area" :fill="`url(#g-${color.replace('#', '')})`" />
      <path v-if="maxPath" :d="maxPath" fill="none" :stroke="color" stroke-width="1" stroke-dasharray="3 3" opacity="0.45" />
      <path :d="path" fill="none" :stroke="color" stroke-width="1.7" stroke-linejoin="round" />
      <!-- 悬停十字线与标记点 -->
      <g v-if="hover">
        <line :x1="hover.x" :y1="PAD.top" :x2="hover.x" :y2="height - PAD.bottom" stroke="#909399" stroke-width="1" stroke-dasharray="4 3" />
        <circle :cx="hover.x" :cy="hover.y" r="3.5" :fill="color" stroke="#fff" stroke-width="1.5" />
      </g>
    </svg>
    <div v-if="hover" class="tip" :style="{ left: tipLeft + 'px' }">
      <div class="tip-time">{{ fmtFull(hover.ts) }}</div>
      <div class="tip-row"><i :style="{ background: color }" />均值 <b>{{ fmt(hover.avg) }}{{ unit }}</b></div>
      <div v-if="showMax" class="tip-row"><i class="dot-max" :style="{ background: color }" />峰值 <b>{{ fmt(hover.max) }}{{ unit }}</b></div>
    </div>
  </div>
  <div v-else class="empty">暂无数据（采样中，5 分钟聚合出首个桶后显示）</div>
</template>

<style scoped>
.trend {
  position: relative;
  display: inline-block;
}
.tick {
  font-size: 10px;
  fill: #909399;
}
.tip {
  position: absolute;
  top: 2px;
  background: rgba(15, 23, 42, 0.92);
  color: #e2e8f0;
  border-radius: 6px;
  padding: 6px 10px;
  font-size: 12px;
  pointer-events: none;
  z-index: 4;
  min-width: 160px;
  box-shadow: 0 2px 8px rgba(0, 0, 0, 0.25);
}
.tip-time {
  color: #94a3b8;
  font-size: 11px;
  margin-bottom: 3px;
}
.tip-row {
  display: flex;
  align-items: center;
  gap: 6px;
  line-height: 1.6;
}
.tip-row b {
  margin-left: auto;
}
.tip-row i {
  width: 8px;
  height: 8px;
  border-radius: 2px;
  display: inline-block;
}
.dot-max {
  opacity: 0.45;
}
.empty {
  color: #909399;
  font-size: 12px;
  line-height: 150px;
  text-align: center;
  width: 100%;
}
</style>
