<script setup lang="ts">
// IDE 底部面板：问题列表（按文件分组，点击跳转）+ 模块文档（跟随光标
// 联动的 Example/Parameters，"确定了模块自动在下方高亮出 example 和
// parameters"）。Example 用只读 Monaco 以 YAML 高亮渲染。
import { onBeforeUnmount, ref, watch } from 'vue'
import type { ModuleMeta } from '../../api'
import type { ProblemItem } from '../../ide/validate'

const props = defineProps<{
  problems: ProblemItem[]
  module: ModuleMeta | null
  panel: 'none' | 'problems' | 'doc'
}>()
const emit = defineEmits<{
  (e: 'update:panel', v: 'none' | 'problems' | 'doc'): void
  (e: 'jump', path: string, line?: number): void
}>()

const tab = ref<'problems' | 'doc'>('problems')
watch(
  () => props.panel,
  (p) => {
    if (p !== 'none') tab.value = p
  },
)
function onTab(t: unknown) {
  emit('update:panel', t as 'problems' | 'doc')
}

// 只读 Monaco 渲染 example。每次切换模块都 createModel——旧 model 必须
// dispose（Monaco model 全局注册表不回收，光标扫过一批模块就漏一批）；
// 面板经 v-if 卸载时编辑器同样要 dispose
const exampleEl = ref<HTMLElement>()
let exampleEditor: any = null
watch(
  () => [props.module?.name, props.panel],
  async () => {
    if (props.panel !== 'doc' || !props.module?.example) return
    const { loadMonaco, editorOptions } = await import('../../ide/monaco')
    const monaco = await loadMonaco()
    if (!exampleEl.value) return
    if (!exampleEditor) {
      exampleEditor = monaco.editor.create(exampleEl.value, {
        ...editorOptions(monaco),
        readOnly: true,
        minimap: { enabled: false },
        scrollBeyondLastLine: false,
        lineNumbers: 'off',
        automaticLayout: true,
        renderLineHighlight: 'none',
        overviewRulerLanes: 0,
      })
    }
    const old = exampleEditor.getModel()
    exampleEditor.setModel(monaco.editor.createModel(props.module.example.trim(), 'yaml'))
    old?.dispose()
  },
  { immediate: true })
onBeforeUnmount(() => {
  exampleEditor?.getModel()?.dispose()
  exampleEditor?.dispose()
  exampleEditor = null
})

const grouped = ref<{ path: string; items: ProblemItem[] }[]>([])
watch(
  () => props.problems,
  (ps) => {
    const m = new Map<string, ProblemItem[]>()
    for (const p of ps) {
      const key = p.path || '（整体）'
      const arr = m.get(key) || []
      arr.push(p)
      m.set(key, arr)
    }
    grouped.value = [...m.entries()]
      .sort((a, b) => a[0].localeCompare(b[0]))
      .map(([path, items]) => ({ path, items: items.sort((x, y) => (x.line || 0) - (y.line || 0)) }))
  },
  { immediate: true },
)
</script>

<template>
  <div class="bottom-panel">
    <div class="panel-head">
      <el-radio-group :model-value="panel === 'none' ? tab : tab" size="small" @update:model-value="onTab">
        <el-radio-button value="problems">问题（{{ problems.length }}）</el-radio-button>
        <el-radio-button value="doc">模块文档</el-radio-button>
      </el-radio-group>
      <div style="flex: 1" />
      <el-button link size="small" @click="emit('update:panel', 'none')">收起 ▾</el-button>
    </div>

    <div v-show="tab === 'problems'" class="panel-body problems">
      <template v-if="grouped.length">
        <div v-for="g in grouped" :key="g.path" class="pgroup">
          <div class="pgroup-title">{{ g.path }}</div>
          <div
            v-for="(p, i) in g.items" :key="i"
            class="pitem" :class="p.level.toLowerCase()"
            @click="emit('jump', p.path || 'deploy.yaml', p.line)"
          >
            <span class="plevel">{{ p.level }}</span>
            <span v-if="p.line" class="pline">L{{ p.line }}</span>
            <span class="pmsg">{{ p.msg }}</span>
          </div>
        </div>
      </template>
      <div v-else class="empty">暂无校验发现（点击工具栏「校验」运行整体校验）</div>
    </div>

    <div v-show="tab === 'doc'" class="panel-body doc">
      <template v-if="module">
        <div class="doc-head">
          <b class="doc-name">{{ module.name }}</b>
          <span class="muted">{{ module.desc }}</span>
          <el-tag v-if="module.rollback" size="small" type="info" effect="plain">
            回滚：{{ ['无', '部分', '全量'][module.rollback] }}
          </el-tag>
          <el-tag v-if="module.read_only" size="small" type="success" effect="plain">只读</el-tag>
        </div>
        <div class="doc-cols">
          <div class="doc-params">
            <table v-if="module.params && module.params.length">
              <thead><tr><th>参数</th><th>类型</th><th>默认</th><th>说明</th></tr></thead>
              <tbody>
                <tr v-for="p in module.params" :key="p.name">
                  <td><code>{{ p.name }}</code></td>
                  <td>{{ p.type }}</td>
                  <td>{{ p.default || '-' }}</td>
                  <td><template v-if="p.enum?.length"><code class="enum-vals">{{ p.enum.join(' / ') }}</code> — </template>{{ p.desc }}</td>
                </tr>
              </tbody>
            </table>
            <div v-else class="muted">该模块无命名参数（free-form 或无参数）</div>
          </div>
          <div v-if="module.example" class="doc-example">
            <div class="muted ex-title">示例（可直接输入模块名补全带出骨架）</div>
            <div ref="exampleEl" class="ex-editor"></div>
          </div>
        </div>
      </template>
      <div v-else class="empty">光标放在任务行上，这里自动展示该模块的 Example 与 Parameters</div>
    </div>
  </div>
</template>

<style scoped>
.bottom-panel {
  border-top: 1px solid #e4e7ed;
  background: #fff;
  display: flex;
  flex-direction: column;
  height: 230px;
  min-height: 120px;
}
.panel-head {
  display: flex;
  align-items: center;
  padding: 4px 10px;
  border-bottom: 1px solid #f0f2f5;
}
.panel-body {
  flex: 1;
  overflow: auto;
  padding: 6px 10px;
}
.pgroup-title {
  font-size: 12px;
  color: #64748b;
  font-family: ui-monospace, Menlo, monospace;
  margin: 6px 0 2px;
}
.pitem {
  display: flex;
  gap: 8px;
  font-size: 12px;
  padding: 3px 6px;
  border-radius: 4px;
  cursor: pointer;
  align-items: baseline;
}
.pitem:hover { background: #f5f7fa; }
.plevel {
  font-size: 10px;
  border-radius: 3px;
  padding: 0 4px;
  flex: none;
}
.pitem.error .plevel { background: #fee2e2; color: #dc2626; }
.pitem.warn .plevel { background: #fef3c7; color: #d97706; }
.pline { color: #94a3b8; font-family: ui-monospace, Menlo, monospace; flex: none; }
.pmsg { color: #334155; word-break: break-all; }
.empty {
  color: #94a3b8;
  font-size: 12px;
  padding: 20px;
  text-align: center;
}
.doc-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
  font-size: 13px;
}
.doc-name { font-family: ui-monospace, Menlo, monospace; }
.doc-cols {
  display: flex;
  gap: 12px;
  height: calc(100% - 30px);
}
.doc-params { flex: 1; overflow: auto; }
.doc-params table {
  border-collapse: collapse;
  font-size: 12px;
  width: 100%;
}
.doc-params th, .doc-params td {
  border: 1px solid #ebeef5;
  padding: 3px 8px;
  text-align: left;
}
.doc-params th { background: #fafbfd; color: #64748b; font-weight: 500; }
.doc-params code { font-family: ui-monospace, Menlo, monospace; }
.doc-params code.enum-vals { color: #476582; background: #f1f5f9; border-radius: 3px; padding: 0 4px; }
.doc-example { flex: 1; display: flex; flex-direction: column; min-width: 0; }
.ex-title { margin-bottom: 2px; }
.ex-editor { flex: 1; min-height: 100px; border: 1px solid #ebeef5; border-radius: 4px; }
.muted { color: #909399; font-size: 12px; }
</style>
