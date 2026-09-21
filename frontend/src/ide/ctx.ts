// YAML 缩进块上下文识别：给补全 provider 判定"光标在哪、该补什么"。
// 纯行扫描（不建 AST）：对人工书写的 playbook YAML 足够可靠，且在语法
// 不完整（正在输入）时也能工作——这正是补全发生的时刻。
//
// 识别两级结构：
//   play 级键（name/hosts/become…）  tasks/handlers/block 列表项之外
//   task 级键（控制键 + 模块键）      tasks/handlers/block/rescue/always 项内
//   模块参数级                        任务 map 里模块键的值块内

export interface KeyCtx {
  kind: 'key'
  level: 'play' | 'task' | 'moduleArgs'
  module?: string // moduleArgs 所属模块；task 级时为该任务已有的模块键
  existing: Set<string> // 当前 mapping 已有键（去重补全用）
}

export interface ValueCtx {
  kind: 'value'
  key: string // 当前行（或 flow 段）的键
  partial: string // 值的已输入部分（去引号）
  quoted: boolean
  module?: string // 所属任务的模块键（枚举值补全用；判不出则缺省）
}

export type YamlCtx = KeyCtx | ValueCtx | null

export const TASK_LIST_KEYS = new Set(['tasks', 'handlers', 'block', 'rescue', 'always'])

const lineIndent = (s: string): number => (s.match(/^ */)![0].length)

// rootIsTaskForm 根级序列形态判定：出现 play 专属键（tasks/handlers/
// serial/strategy/根级 hosts）→ play 形态；出现模块键 → 裸任务形态；
// 两者都无（只有 name:，或全新文件 "- cha"）按调用方缺省——chart 相位
// 默认任务、playbook 编辑器默认 play。
function rootIsTaskForm(lines: string[], controlKeys: Set<string>, playKeys: Set<string>, defaultIsTask: boolean): boolean {
  let hasModule = false
  for (const l of lines) {
    const m = l.match(/^(?:- | {2})([\w.][\w.-]*):/)
    if (!m) continue
    if (m[1] === 'tasks' || m[1] === 'handlers' || m[1] === 'serial' || m[1] === 'strategy' || m[1] === 'hosts') {
      return false
    }
    if (!controlKeys.has(m[1]) && !playKeys.has(m[1])) hasModule = true
  }
  return hasModule ? true : defaultIsTask
}

// 解析一行：dash 缩进 / mapping 键起点缩进 / 键名
function parseLine(line: string): { dashIndent: number | null; keyIndent: number; key: string | null; hasColon: boolean } {
  const m = line.match(/^(\s*)(-\s+)?([\w.][\w.-]*)?(\s*:\s*)?(.*)$/)
  if (!m) return { dashIndent: null, keyIndent: lineIndent(line), key: null, hasColon: false }
  const indent = m[1].length
  const hasDash = !!m[2]
  const keyStart = indent + (m[2] ? m[2].length : 0)
  return {
    dashIndent: hasDash ? indent : null,
    keyIndent: keyStart,
    key: m[3] || null,
    hasColon: !!m[4],
  }
}

// yamlCtx 判定光标上下文。controlKeys/playKeys 用于区分模块键。
export function yamlCtx(
  text: string,
  position: { lineNumber: number; column: number },
  controlKeys: Set<string>,
  playKeys: Set<string>,
  rootDefaultIsTask = true,
): YamlCtx {
  const lines = text.split('\n')
  const li = position.lineNumber - 1
  if (li < 0 || li >= lines.length) return null
  const line = lines[li]
  const before = line.slice(0, position.column - 1)

  // ---- 值位置：`key: <cursor>…`（含 flow map 段）----
  const vm = before.match(/^[^:#]*?([\w.][\w.-]*)\s*:\s*(.*)$/)
  if (vm) {
    const rest = line.slice(before.length)
    // flow map：{src: a, dest: b} 里光标在某个键值段
    const flow = before.match(/[{,]\s*([\w.][\w.-]*)\s*:\s*([^,{}]*)$/)
    if (flow) {
      return {
        kind: 'value', key: flow[1],
        partial: flow[2].replace(/^['"]/, ''), quoted: /^['"]/.test(flow[2]),
      }
    }
    // 块值（整行冒号后）：确认这行不是"正在输入的键"（before 以冒号结尾的
    // 情况已排除——上面正则要求冒号后有内容或空，光标在冒号后即值位置）
    if (!/^\s*\|/.test(rest) || before.trimEnd().endsWith(':')) {
      const afterColon = vm[2]
      // 模块归属：光标挪到键名首列跑一次键位判定（该位置行内只有缩进/
      // dash，走键分支），拿到所属任务模块——枚举值补全（state/mode 等）
      // 依赖它定位参数所属模块
      const keyCol = before.lastIndexOf(vm[1]) + 1
      const keyCtx = yamlCtx(text, { lineNumber: position.lineNumber, column: Math.max(keyCol, 1) },
        controlKeys, playKeys, rootDefaultIsTask)
      return {
        kind: 'value', key: vm[1],
        partial: afterColon.replace(/^['"]/, ''), quoted: /^['"]/.test(afterColon),
        module: keyCtx && keyCtx.kind === 'key' ? keyCtx.module : undefined,
      }
    }
  }

  // ---- 键位置：行首到光标是缩进 + 可选 dash + 键名前缀 ----
  if (!/^(\s*)(-\s+)?([\w.-]*)$/.test(before)) return null
  const cur = parseLine(line)
  const curIsDash = cur.dashIndent !== null
  const mappingIndent = cur.keyIndent

  // 向上收集：同缩进的键（existing）与更浅缩进的祖先。
  // 注意：当前行正在输入的键（cur.key）不算 existing——它就是补全目标
  // 本身，算进去会被误当"已有模块键"而只查参数。
  const existing = new Set<string>()
  let level: 'play' | 'task' | 'moduleArgs' = 'play'
  let module: string | undefined
  let underTaskList = false

  let sawStructural = false
  for (let i = li - 1; i >= 0; i--) {
    const l = lines[i]
    if (!l.trim() || l.trimStart().startsWith('#')) continue
    sawStructural = true
    const p = parseLine(l)
    if (p.keyIndent >= mappingIndent && p.dashIndent === null) {
      // 光标在 dash 行上时，上方同列的键属于上一个同级任务（新任务从
      // dash 行开始，上方内容不是本项的），不收进 existing——否则上个
      // 任务的模块键会让本行误判"已写模块"、出不了模块名补全
      if (p.key && !curIsDash) existing.add(p.key)
      continue
    }
    if (p.dashIndent !== null && p.dashIndent >= mappingIndent) {
      continue // 同层或更深的其他列表项
    }
    if (p.dashIndent !== null && p.dashIndent < mappingIndent) {
      // 上层列表项行（"- copy:" / "- name: x"）。光标在续行上时它是
      // 当前项（键收进 existing 供去重/模块推断）；光标自己在 dash 行上
      // 时它是上一个同级任务——键不能进上下文（否则上个任务的模块键
      // 会被当成"当前任务已写模块"，模块名补全就不出了）
      if (!curIsDash && p.key && !controlKeys.has(p.key) && !playKeys.has(p.key) && p.key !== 'name' && mappingIndent > p.keyIndent) {
        return { kind: 'key', level: 'moduleArgs', module: p.key, existing }
      }
      if (!curIsDash && p.key) existing.add(p.key)
      let foundParent = false
      for (let j = i - 1; j >= 0; j--) {
        const l2 = lines[j]
        if (!l2.trim() || l2.trimStart().startsWith('#')) continue
        const p2 = parseLine(l2)
        // <= 而非 <：`tasks:` 与其列表项同缩进是合法 YAML（本仓库
        // 示例即此写法），漏判会把任务续行当 play 级、出不了模块补全
        if (p2.keyIndent <= p.dashIndent! && p2.key) {
          if (TASK_LIST_KEYS.has(p2.key)) underTaskList = true
          foundParent = true
          break
        }
        if (p2.dashIndent !== null && p2.dashIndent < p.dashIndent!) {
          break // 到了外层列表（根列表项 = play）
        }
      }
      // 到根都没找到任务列表父键：根级序列本身是裸任务形态（相位文件
      // 直接写任务）→ 仍是任务级
      if (!foundParent && rootIsTaskForm(lines, controlKeys, playKeys, rootDefaultIsTask)) {
        underTaskList = true
      }
      break
    }
    if (p.keyIndent < mappingIndent && p.key) {
      // 更浅的 `key:` 行：mapping 是它的值块
      if (TASK_LIST_KEYS.has(p.key)) {
        underTaskList = true
        break
      }
      if (!controlKeys.has(p.key) && !playKeys.has(p.key) && p.key !== 'strategy' && p.key !== 'loop_control') {
        module = p.key // 模块参数块（when/vars/environment 等控制键的值块不给键补全）
        level = 'moduleArgs'
      } else {
        return null // 控制键的值块（when/vars/…）：不是键补全位置
      }
      break
    }
    if (p.dashIndent !== null && p.dashIndent >= mappingIndent) continue // 同层其他项
  }

  if (!sawStructural && rootIsTaskForm(lines, controlKeys, playKeys, rootDefaultIsTask)) {
    underTaskList = true
  }
  if (level === 'moduleArgs') return { kind: 'key', level, module, existing }

  // task 级：任务 map 里第一个非控制键 = 模块键
  if (underTaskList) {
    level = 'task'
    for (const k of existing) {
      if (!controlKeys.has(k) && !playKeys.has(k) && k !== 'name') {
        module = k
        break
      }
    }
  }
  return { kind: 'key', level, module, existing }
}

// inTemplateExpr 判定光标是否在 {{ … }} 内，返回表达式已输入部分
//（含前导 '.'；不含 {{）。跨行模板罕见，按单行处理足够。
export function inTemplateExpr(lineBefore: string): string | null {
  const open = lineBefore.lastIndexOf('{{')
  if (open < 0) return null
  const after = lineBefore.slice(open + 2)
  if (after.includes('}}')) return null // 已闭合
  return after
}

// 模板表达式的补全入口形态：
//   '.app.po'  → 路径续补（against 前缀解析出的子对象）
//   'eq'       → 函数/根变量候选
//   'include "'|'template "' → helper 名
export function templateExprKind(expr: string): { kind: 'path' | 'token' | 'helper' } {
  if (/^(include|template)\s*"([^"]*)$/.test(expr)) return { kind: 'helper' }
  if (/^[\s]*\./.test(expr)) return { kind: 'path' }
  return { kind: 'token' }
}
