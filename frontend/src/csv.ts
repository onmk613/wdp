// 批量导入 CSV：固定列序、半角逗号分隔；单元格内多值（池/组/标签）用
// 分号分隔。刻意不做带引号的完整 RFC4180——列值（主机名/IP/标签键值）
// 不含逗号，简单 split 更可预期；Excel 编辑兼容靠 BOM（模版已带，解析
// 前剥掉）。

export interface ParsedRow<T> {
  line: number // 源文本行号（1 起，报错定位用）
  value: T | null // 解析失败为 null
  error?: string
}

export interface HostCSVRow {
  name: string // 空 = 取 address（解析时已回填）
  address: string
  agentPort: number // 空 = 7602
  pools: string[]
  groups: string[]
  labels: Record<string, string>
}

export interface SSHCSVRow {
  name: string // 空 = 取 address（解析时已回填）
  address: string
  sshPort: number // 空 = 0（= 用对话框共享值/默认 22）
  user: string // 空 = 用对话框共享用户
  password: string // 空 = 用对话框共享密码
}

// 去注释/空行，识别并跳过模版头行（首列 name、次列 address 即视为头）。
// 返回携带原始行号（与文本框行号一致，报错定位用）。
function dataLines(text: string, header: string[]): { text: string; line: number }[] {
  const out: { text: string; line: number }[] = []
  const raws = text.replace(/^\uFEFF/, '').split(/\r?\n/)
  for (let i = 0; i < raws.length; i++) {
    const line = raws[i].trim()
    if (!line || line.startsWith('#')) continue
    if (out.length === 0) {
      const cells = line.split(',').map((c) => c.trim().toLowerCase())
      if (cells[0] === 'name' && cells[1] === header[1]) continue
    }
    out.push({ text: line, line: i + 1 })
  }
  return out
}

function parsePort(s: string): number | null {
  if (!/^\d+$/.test(s) || +s < 1 || +s > 65535) return null
  return +s
}

function splitMulti(s: string): string[] {
  return s.split(';').map((v) => v.trim()).filter(Boolean)
}

function parseLabelsCell(s: string): Record<string, string> | null {
  const out: Record<string, string> = {}
  for (const seg of splitMulti(s)) {
    const eq = seg.indexOf('=')
    if (eq < 0) {
      if (!seg) return null
      out[seg] = ''
      continue
    }
    const k = seg.slice(0, eq).trim()
    if (!k) return null
    out[k] = seg.slice(eq + 1).trim()
  }
  return out
}

// 手动添加：name,address,agent_port,pools,groups,labels
// 只填一列时视为纯地址（name 默认 = address，其余全默认）。
export function parseHostCSV(text: string): ParsedRow<HostCSVRow>[] {
  const seen = new Map<string, number>() // 主机名 → 首次出现的行号（查重）
  return dataLines(text, ['name', 'address']).map(({ text: line, line: no }) => {
    const cells = line.split(',').map((c) => c.trim())
    if (cells.length === 1) cells.push(cells[0]) // 纯 IP 行：name = address = IP
    if (cells.length > 6) return { line: no, value: null, error: `列数过多（${cells.length} > 6）` }
    const [name, address, port = '', pools = '', groups = '', labels = ''] = cells
    if (!address) return { line: no, value: null, error: 'address 必填' }
    const agentPort = port ? parsePort(port) : 7602
    if (agentPort === null) return { line: no, value: null, error: `agent_port 非法：${port}` }
    const labelMap = parseLabelsCell(labels)
    if (labelMap === null) return { line: no, value: null, error: `标签格式：k=v;k2=v2（可只写键），实际 ${labels}` }
    const finalName = name || address
    if (seen.has(finalName)) return { line: no, value: null, error: `主机名与第 ${seen.get(finalName)} 行重复` }
    seen.set(finalName, no)
    return {
      line: no,
      value: { name: finalName, address, agentPort, pools: splitMulti(pools), groups: splitMulti(groups), labels: labelMap },
    }
  })
}

// SSH 安装：name,address,ssh_port,user,password（user/password 空 = 用共享值）
// 只填一列时视为纯地址（name 默认 = address）。
export function parseSSHCSV(text: string): ParsedRow<SSHCSVRow>[] {
  const seen = new Map<string, number>()
  return dataLines(text, ['name', 'address']).map(({ text: line, line: no }) => {
    const cells = line.split(',').map((c) => c.trim())
    if (cells.length === 1) cells.push(cells[0]) // 纯 IP 行：name = address = IP
    if (cells.length > 5) return { line: no, value: null, error: `列数过多（${cells.length} > 5）` }
    const [name, address, port = '', user = '', password = ''] = cells
    if (!address) return { line: no, value: null, error: 'address 必填' }
    const sshPort = port ? parsePort(port) : 0
    if (sshPort === null) return { line: no, value: null, error: `ssh_port 非法：${port}` }
    const finalName = name || address
    if (seen.has(finalName)) return { line: no, value: null, error: `主机名与第 ${seen.get(finalName)} 行重复` }
    seen.set(finalName, no)
    return { line: no, value: { name: finalName, address, sshPort, user, password } }
  })
}

// ---- 模版（\uFEFF BOM：Excel 双击打开不乱码）----

export const hostCSVTemplate =
  '\uFEFF' +
  `# 手动添加主机：name,address,agent_port,pools,groups,labels
# 多值用分号分隔（pools=web;prod，labels=env=prod;rack=a1）；整行只写一个 IP 也可以（name=IP，其余默认）；# 开头为注释
name,address,agent_port,pools,groups,labels
web1,192.168.1.11,7602,web;prod,api,env=prod;rack=a1
web2,192.168.1.12,,web;prod,,env=prod
db1,192.168.1.21,,,db,
192.168.1.31
`

export const sshCSVTemplate =
  '\uFEFF' +
  `# SSH 安装 agent：name,address,ssh_port,user,password
# 整行只写一个 IP 也可以（name=IP，端口/用户/密码用共享设置）；user/password 留空 = 用对话框里的共享设置；# 开头为注释
name,address,ssh_port,user,password
web1,192.168.1.11,22,root,
web2,192.168.1.12,,,
db1,192.168.1.21,,root,another-pass
192.168.1.31
`

export function downloadText(filename: string, content: string) {
  const blob = new Blob([content], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  // 延迟 revoke：click 后同步 revoke 在部分浏览器会中断尚未开始的下载
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

// 读取上传的 CSV 文件为文本（交给 textarea 继续编辑/预览）。
export function readCSVFile(file: File): Promise<string> {
  return file.text()
}
