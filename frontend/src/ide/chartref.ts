// 光标所在任务的 chart 引用定位（自 complete.ts 的相位域判定抽出）：
// tasks_from/phase 值补全需要知道当前任务引用的是哪个 chart，才能给出
// 正确的相位候选域。独立成纯函数模块——不经 schemas/monaco 的值导入
// 链，单测无需拉起 monaco。
//
// 行文法与 ctx.ts 的 parseLine 同式（缩进 / 可选 dash / 键名 / 冒号 /
// 余文），向上扫描的缩进语义一致：同块键行、同层或更深的列表项跳过、
// 更浅的 dash 行是本项（任务块）起点、更浅的键行意味着光标在其值块内。

const LINE_RE = /^(\s*)(-\s+)?([\w.][\w.-]*)?(\s*:\s*)?(.*)$/

// taskChartRef 光标所在任务块里的 chart 引用名。三种形态都认：
//   标量缩写   - chart: nginx
//   map 展开   - chart: 值块内的 name: nginx
//   同行 flow  chart: {name: nginx, phase: …}
// 光标不在任务块内、本任务不是 chart 引用、或引用名是模板变量等非标量
// 形态时返回 undefined——调用方回退全局扫描启发式。
export function taskChartRef(
  text: string,
  position: { lineNumber: number; column: number },
): string | undefined {
  const lines = text.split('\n')
  const li = position.lineNumber - 1
  if (li < 0 || li >= lines.length) return undefined
  // 当前行（含 dash）的 mapping 键列：块内其他行都与它比缩进
  const cur = LINE_RE.exec(lines[li])!
  const mappingIndent = cur[1].length + (cur[2] ? cur[2].length : 0)
  for (let i = li; i >= 0; i--) {
    const raw = lines[i]
    if (!raw.trim() || raw.trimStart().startsWith('#')) continue
    const m = LINE_RE.exec(raw)!
    const indent = m[1].length
    const keyStart = indent + (m[2] ? m[2].length : 0)
    const key = m[3] || null
    if (m[2]) {
      if (indent >= mappingIndent) continue // 同层或更深的其他列表项
      // 本项 dash 行（缩进比当前 mapping 浅）：当前任务块的起点
      return key === 'chart' ? chartRefValue(m[5] || '', lines, i, keyStart) : undefined
    }
    if (keyStart >= mappingIndent) {
      // 同块键行（含光标行自身）：chart 键即引用所在
      if (key === 'chart') return chartRefValue(m[5] || '', lines, i, keyStart)
      continue
    }
    // 更浅的键行：光标在其值块内——chart 的值块（map 展开）本身就是
    // 引用上下文；其余（play 键/tasks:/其他控制键的值块）不是，交回退
    return key === 'chart' ? chartRefValue(m[5] || '', lines, i, keyStart) : undefined
  }
  return undefined
}

// chartRefValue chart: 键的引用名：标量缩写 > 同行 flow > map 展开块内
// 的 name: 标量（块边界 = 缩进不再比 chart 键深）
function chartRefValue(rest: string, lines: string[], keyLine: number, keyIndent: number): string | undefined {
  const scalar = rest.trim().match(/^([A-Za-z0-9][\w.-]*)$/)
  if (scalar) return scalar[1]
  const flow = rest.match(/[{,]\s*name:\s*([A-Za-z0-9][\w.-]*)/)
  if (flow) return flow[1]
  for (let i = keyLine + 1; i < lines.length; i++) {
    const raw = lines[i]
    if (!raw.trim() || raw.trimStart().startsWith('#')) continue
    const m = LINE_RE.exec(raw)!
    const keyStart = m[1].length + (m[2] ? m[2].length : 0)
    if (keyStart <= keyIndent) break // 出了 chart 值块
    if (m[3] === 'name') {
      const v = (m[5] || '').trim().match(/^([A-Za-z0-9][\w.-]*)$/)
      if (v) return v[1]
    }
  }
  return undefined
}
