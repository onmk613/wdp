// 后端 API 封装：JSON fetch + 统一错误口径 + 401 全局事件。
// 任何请求收到 401 都派发 wdp-unauthorized（App 切回登录视图）。

export interface Host {
  ID: number
  Name: string
  Address: string
  AgentPort: number
  Pools: string[] | null
  Groups: string[] | null
  Labels: string
  Status: string
  LastSeenAt: string
  CreatedAt: string
  UpdatedAt: string
}

export interface Pool {
  ID: number
  Name: string
  Note: string
  Members: number
  CreatedAt: string
}

export interface GroupEntry {
  ID: number
  Name: string
  Note: string
  CreatedAt: string
}

export interface LabelDef {
  ID: number
  Key: string
  Note: string
  CreatedAt: string
}

export interface BatchResult {
  Row?: number // 批量建档时 = 提交数组中的序号（1 起）
  ID: number
  Name: string
  OK: boolean
  Detail?: string
  Retired?: boolean
}

export interface BatchResponse {
  results: BatchResult[]
  ok: number
  failed: number
}

export interface EnrollTokenResp {
  token: string
  expires_in: number
  command: string
}

export interface ProbeResult {
  status: string
  scheme?: string
  error?: string
  hostname?: string
  version?: string
  goos?: string
  arch?: string
  cert_not_after?: string
  checked_at: string
}

// 主机详情（GET /api/hosts/{id}/facts）：探活快照 + setup 模块采集的 facts
export interface HostFactsResponse {
  probe: ProbeResult
  facts?: {
    hostname: string
    kernel: string
    arch: string
    default_ipv4: string
    euid: number
    cpus: number
    memory_mb: number
    os: { id: string; name: string; version: string; family: string }
    disk: { total_bytes: number; used_bytes: number; avail_bytes: number; use_percent: number }
  }
  error?: string
}

export async function api<T>(method: string, url: string, body?: unknown): Promise<T> {
  const opt: RequestInit = { method, headers: { Accept: 'application/json' } as Record<string, string> }
  if (body !== undefined) {
    ;(opt.headers as Record<string, string>)['Content-Type'] = 'application/json'
    opt.body = JSON.stringify(body)
  }
  const resp = await fetch(url, opt)
  if (resp.status === 401) {
    window.dispatchEvent(new Event('wdp-unauthorized'))
    throw new Error('登录已失效')
  }
  const data = (await resp.json().catch(() => ({}))) as T & { error?: string }
  if (!resp.ok) {
    throw new Error(data?.error || `HTTP ${resp.status}`)
  }
  return data
}

export interface App {
  ID: number
  Name: string
  Note: string
  LatestVersion: string
  Pools: string[] | null
  Groups: string[] | null
  Labels: string
  VersionCount: number
  CreatedAt: string
  UpdatedAt: string
}

export interface AppVersion {
  ID: number
  AppID: number
  Version: string
  Sha256: string
  Size: number
  Note: string
  Phases: string[] | null
  CreatedAt: string
}

export interface Run {
  ID: number
  Kind: string
  AppID: number
  AppName: string
  Version: string
  Phase: string
  Seq: number
  Status: string
  Selector: string
  Summary: string
  User: string
  StartedAt: string
  FinishedAt: string
}

// 用户与会话管理（user:manage）
export interface UserScope {
  verb: string
  kind: string // '' = 全部 | pool | group | label
  value: string
}

export interface User {
  ID: number
  Name: string
  Role: string
  Disabled: boolean
  CreatedAt: string
  Scopes: UserScope[] | null
  Online: boolean
}

export interface SessionInfo {
  token: string
  user: string
  expires: string
}

// 操作审计（GET /api/audit）
export interface AuditLog {
  ID: number
  User: string
  Action: string
  Object: string
  Name: string
  Detail: string
  IP: string
  CreatedAt: string
}

// 主机健康告警（GET /api/alerts；页面红黄标记，真正告警走 Prometheus）
export interface HostAlert {
  HostID: number
  HostName: string
  Kind: string // cpu / mem / fs / offline
  Level: string // warn / crit
  Detail: string
  Value: number
  UpdatedAt: string
}

// Prometheus 文本格式的解析样本（GET /api/hosts/{id}/metrics?format=json）
export interface MetricSample {
  name: string
  labels?: Record<string, string>
  value: number
}

// 5 分钟聚合桶（GET /api/hosts/{id}/series）
export interface SeriesPoint {
  ts: number
  avg: number
  max: number
}

// 该主机最近的执行任务（GET /api/hosts/{id}/tasks）
export interface HostTaskItem {
  RunID: number
  AppName: string
  Kind: string
  Task: string
  Module: string
  Status: string
  Changed: boolean
  Detail: string
  StartAt: string
}

export interface RunTask {
  ID: number
  RunID: number
  Play: string
  Task: string
  Module: string
  Host: string
  Status: string
  Changed: boolean
  Detail: string
}

// 注意：字段名与后端 JSON 键一一对应（Go 侧 json tag 为小写，
// 曾因写成 PascalCase 导致执行结果全部 undefined——页面显示 rc=undefined/无输出）
export interface ExecHostResult {
  id: number
  name: string
  code: number
  stdout: string
  stderr: string
  err?: string
}

// multipart 上传（应用 tgz）
export async function upload(url: string, fields: Record<string, string>, file: File): Promise<any> {
  const fd = new FormData()
  for (const [k, v] of Object.entries(fields)) fd.append(k, v)
  fd.append('tgz', file)
  const resp = await fetch(url, { method: 'POST', body: fd, headers: { Accept: 'application/json' } })
  if (resp.status === 401) {
    window.dispatchEvent(new Event('wdp-unauthorized'))
    throw new Error('登录已失效')
  }
  const data = await resp.json().catch(() => ({}))
  if (!resp.ok) throw new Error(data?.error || `HTTP ${resp.status}`)
  return data
}

export interface SpecFile {
  path: string
  content?: string
  binary?: boolean
  size: number
}

// 模块自描述（GET /api/modules）：编辑器动态表单的数据源，新增模块自动出现
export interface ModuleParamDoc {
  name: string // "(free-form)" = 简写参数（如 shell: uptime）
  type: string // string / list / bool / int / mode / map
  default?: string
  desc: string
  enum?: string[] | null // 固定值域（后端解析白名单同源；值补全/文档消费）
}

export interface ModuleMeta {
  name: string
  desc: string
  free_form: boolean
  params: ModuleParamDoc[] | null
  example?: string
  rollback: number // 0=无 1=部分 2=全量
  read_only: boolean
}

export interface AppSpec {
  name: string
  version: string
  description: string
  values_yaml: string
  deploy_yaml: string
  files: SpecFile[] | null
  pools: string[] | null
  groups: string[] | null
  labels: string
}

// ---- IDE 支撑（schema / validate / draft）----

// GET /api/schema：结构元数据（与后端 model.FieldSection 对应，无 json
// tag 按导出名序列化）
export interface FieldDoc {
  Name: string
  Type: string
  Default?: string
  Desc: string
}

export interface FieldSection {
  Title: string
  Fields: FieldDoc[]
  Example?: string
}

export interface SchemaMeta {
  task: FieldSection[]
  play: FieldSection[]
  chart: FieldSection[]
  builtin_vars: string[]
  template_funcs: string[]
}

// POST /api/apps/validate 的发现（line 为后端结构化行号，0/缺省 = 未知）
export interface ValidateIssue {
  level: string // ERROR / WARN
  path: string // chart 内相对路径（'' = 整体性问题）
  msg: string
  line?: number
}

// 保存/校验请求体（specReq 的 IDE 形态；三件套以 verbatim 文件提交）
export interface SpecSaveBody {
  name?: string // 新建模式必填
  version: string
  description: string
  files: { path: string; content: string; size?: number }[]
  delete_files: string[]
  base_version: string
  pools: string[]
  groups: string[]
  labels: string
}

// GET /api/apps/draft
export interface DraftResp {
  payload: string
  base_version: string
  updated_at: string
}

// ---- 具名 API（资源级封装层）----
// 视图不散落字符串 URL 与请求形状：路径、参数、响应类型集中在这里，
// 接口增删改只动这一处（调用方只见函数签名与类型）。新功能一律走这
// 层，存量内联调用在触及时逐步迁移。

// GET /api/apps/drafts：草稿箱列表（当前用户）
export interface AppDraftItem {
  key: string // "<appID>"（编辑中）| "new:<name>"（新建中）
  kind: 'new' | 'edit'
  app_name: string
  app_gone: boolean // 底本应用已删除（草稿仅可清理，不可继续编辑）
  base_version: string
  updated_at: string
}

export function listAppDrafts(): Promise<AppDraftItem[]> {
  return api('GET', '/api/apps/drafts')
}

export function deleteAppDraft(key: string): Promise<void> {
  return api('DELETE', `/api/apps/draft?key=${encodeURIComponent(key)}`)
}

// POST /api/playbook/validate：裸 playbook 内容级校验（单文件，非 chart）
export async function validatePlaybook(content: string): Promise<ValidateIssue[]> {
  const resp = await api<{ issues: ValidateIssue[] | null }>('POST', '/api/playbook/validate', { content })
  return resp.issues || []
}

// POST /api/apps/spec：图形化新建应用（chart.yaml 须在 files 里，版本与
// 之对账；deploy.yaml 支持 play 形态——playbook 内容可直接作为部署相位）
export function createAppFromSpec(body: SpecSaveBody): Promise<App> {
  return api('POST', '/api/apps/spec', body)
}

// GET /api/apps/{id}/download：下载版本制品（tgz；cookie 会话直接导航即可，
// Content-Disposition: attachment 触发浏览器下载而不离开当前页）
export function appDownloadURL(appID: number, version?: string): string {
  return `/api/apps/${appID}/download${version ? `?version=${encodeURIComponent(version)}` : ''}`
}

// ---- run 事件流（SSE）----
// 执行状态变化的服务端推送：取代高频全量轮询（读放大主源）。返回断开
// 函数；EventSource 同源自动带会话 cookie，断线自动重连，重连后靠
// 调用方的兜底轮询补齐错过的状态。
export interface RunEvent {
  id: number
  status: string
  summary?: string
}

export function subscribeRuns(onEvent: (e: RunEvent) => void): () => void {
  const es = new EventSource('/api/runs/stream')
  es.addEventListener('run', (ev) => {
    try {
      onEvent(JSON.parse((ev as MessageEvent).data) as RunEvent)
    } catch { /* 坏载荷忽略：兜底轮询会补 */ }
  })
  return () => es.close()
}
