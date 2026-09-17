# 16 --host-config 与结构自描述方案

> 状态：**§1 未实施；§2 已实施 host/task/chart 三个域**（`wdp schema
> host|task|chart`，含对账测试与 `--json`；play 域与 JSON Schema 导出未做）。
> 两部分相互独立，可分期落地。

## 1. `--host-config`：内联主机的连接配置

### 1.1 背景与问题

`run` / `adhoc` 的 `--hosts` 内联主机（免 inventory 文件）当前连接参数是三层兜底：

| 层 | 来源 | 覆盖 |
|---|---|---|
| spec 本身 | `host:port`、区间展开（inventory/specs.go parseHostSpec） | 端口 |
| wdp.cfg | `[run].conn`（缺省 push）、`[ssh].user`、`[ssh].connect_timeout`、`[ssh].host_key_check`（cli connDefaults → conn.Defaults） | 连接类型、用户、超时、指纹校验 |
| ~/.ssh/config | sshcfg.FillFromSSHConfig | user / port / IdentityFiles（key 路径） |

**缺口**：只能在 inventory 逐主机配的字段内联模式全给不了——`password`/`password_env`、
显式 `key_path`、`key_passphrase`、`via` 中继、以及 **`agent_url` 与 agent TLS 材料**
（内联主机当前无法走 agent 通道）。

反面方案与否决理由：

- **逐字段加 flag**（`--ssh-user --ssh-key --agent-url...`）：run 已有 12 个 flag，
  会持续膨胀；与 wdp.cfg、~/.ssh/config 大量重复。正交字段集合应一个入口。
- **flag 分组**：cobra 的 `AddGroup` 是子命令分组，flag 无分组机制；最多
  `Flags().SortFlags = false` 让 help 按声明序聚类。
- **条件补全**（传了 `--hosts` 才出现某些 flag）：cobra flag 名补全无过滤钩子，
  且命令树构造早于 flag 解析，不可行。

### 1.2 方案

单一**可重复** flag，仅内联模式生效，键值套用到全部内联主机：

```sh
# ssh 场景：换用户换钥匙
wdp adhoc -m shell -a uptime --hosts 10.8.2.101-104 \
    --host-config conn=ssh --host-config user=deploy \
    --host-config key_path=~/.ssh/deploy.id_ed25519

# agent 场景：内联主机走 agent 通道（当前完全做不到）
wdp run ./myapp --hosts 10.8.2.11 \
    --host-config conn=agent --host-config agent_url=http://10.8.2.11:7602 \
    --host-config tls=true --host-config cert_file=me.crt --host-config key_file=me.key
```

设计要点：

1. **键名与 inventory 主机条目完全一致**（零新概念，`conn/user/port/password/
   password_env/key_path/agent_url/tls/...`）——字段语义、校验、env: 敏感值约定
   全部复用 inventory 已有实现，不另立一套。
2. **互斥语义**：`-i` 用户显式指定时传 `--host-config` 直接报错（与 `--hosts`
   互斥检查同位置：cli/common.go hostSource）。
3. （可选糖）`--hosts 'deploy@10.8.2.101'` 支持 `user@` 前缀，覆盖最高频的
   临时换用户场景。

### 1.3 实现要点

- **flag 挂载**：`run` / `adhoc` 各声明 `--host-config`（StringArray），传入
  `hostSource(hostsInline, hostConfig)`（cli/common.go）。
- **字段套用**：把 inventory/host.go `buildHost` 的字段 switch 抽出可复用函数
  （如 `ApplyHostFields(h *model.Host, fields map[string]any) error`），
  `HostsFromSpecs` 在 parseHostSpec 构造 Host 后套用——显式性记账
  （explicitUser/explicitPort/explicitKeyPath，决定 ~/.ssh/config 兜底是否让位）
  与 buildHost 同一套语义。
- **解析顺序**：spec 端口（`host:port`）> `--host-config` > wdp.cfg > ~/.ssh/config。
- **测试**：互斥报错；字段生效（key_path/agent_url）；`password_env` 解引用；
  `--host-config port` 与 spec 端口冲突时 spec 优先；-i 模式报错；ssh config
  兜底不被无关注入。

### 1.4 验收标准

- `--hosts` + `--host-config conn=agent agent_url=...` 能跑通 agent 通道内联执行；
- inventory 模式传 `--host-config` 报错且错误信息说明用法；
- `wdp run --help` 中 `--hosts` 与 `--host-config` 相邻展示（SortFlags=false）。

---

## 2. 结构自描述：`wdp schema`（字段速查，同构 `wdp module`）

### 2.1 问题

`wdp module` 已实现"参数文档与实现同源、永不漂移"（UsageProvider + ParamDoc，
各模块自带 Params/Example）。但写 playbook / inventory / chart.yaml 时的**结构字段**
（task 的 when/loop/delegate_to、host 条目的 password_env/via/TLS、chart.yaml 的
phases/check_mode）仍需翻文档——文档与代码分离，新增字段容易漏更。

### 2.2 现状盘点：各结构的"唯一真相"在哪

| 结构 | 唯一真相 | 元数据 |
|---|---|---|
| task 控制键 | playbook/taskdoc.go `TaskFieldSections()`（`taskKeys` 由它**派生**）+ parseTask* 系列函数 | **有**（`wdp schema task`） |
| play 键 | playbook/playbook.go parsePlayNode 的 switch | 无 |
| inventory host 条目 | inventory/hostdoc.go `HostFieldSections()` + host.go `buildHost`/`hostKeyApplier` | **有**（`wdp schema host`） |
| chart.yaml | chart/chartdoc.go `ChartFieldSections()` + chart/meta.go 结构体 + yaml tag | **有**（`wdp schema chart`） |
| 模块参数 | module 各实现的 Params() | **有**（wdp module） |

### 2.3 方案对比

**A. 反射 yaml tag / struct 自动生成** —— 否决。嵌套结构（block/rescue/always）、
开放键（模块名是动态 key）、跨层联动（play.become 被 task 覆盖、--diff 自动开
--check）、约束语义（serial 表达式）都表达不了；长中文文档塞 tag 不可维护。

**B. 显式 FieldDoc 注册表（推荐）** —— 与 module 机制同构：定义
`FieldDoc{Name, Type, Default, Desc, Example}`，为 task/play/host/chart 各建
字段表，新命令 `wdp schema <task|play|host|chart>` 输出表格 + 示例片段。
漂移风险用**对账测试**堵死（这是 A 的自动性优势的对冲）：

```go
// 双向对账：taskKeys 有而 TaskFields() 缺 → 报错；TaskFields() 有而
// taskKeys 不认识 → 报错（防止文档凭空捏造字段）
```

对账点：`taskKeys ↔ TaskFields()`、`buildHost case ↔ HostFields()`、
`chart.Meta yaml tag ↔ ChartFields()`。

**C. JSON Schema 导出（B 的增值项）** —— B 的数据可导出 JSON Schema
（`wdp schema host --json-schema > wdp-schema.json`），配合编辑器 YAML 插件
做到**写时补全**，比 CLI 查询更贴近"不用翻文档"。开放模块键部分
（`shell:`/`file:` 是动态 key）在 schema 中留白或仅列模块名。

### 2.4 推荐形态

```sh
wdp schema task          # 任务控制属性全表（when/loop/until/delegate_to...）
wdp schema host          # inventory 主机条目字段全表（含 env: 敏感值约定）
wdp schema chart         # chart.yaml 字段 + 生命周期相位属性（已实施）
wdp schema play          # play 键（未实施）
wdp schema task --json   # 机器可读（编辑器插件/脚本）
```

- 实现：新 internal/schemadoc（FieldDoc 类型 + 四张字段表 + 对账测试），
  CLI 侧复用 `wdp module` 的表格渲染（outPrinter）与 `--json` 惯例；
  补全用 ValidArgsFunction 候选域名（已实施 host/task/chart 三个；play 未做）。
- 联动：lint 的 unknown-key 报错信息附 `wdp schema task` 提示
  （与 adhoc 的 "run `wdp module` for the list" 同惯例）。

### 2.5 价值排序与工作量

1. **host 最值得**：字段最多且最隐蔽（password_env/key_passphrase_env/
   TLS 五件套/insecure_skip_verify），此前是查代码才知道存在——**已实施**；
2. task 次之：docs/05 已全，但 CLI 内查更快，且与 §1 的 --host-config 键名
   互为印证——**已实施**；
3. chart.yaml 第三：字段少但相位属性易漏（release/record/clears_marker/
   values_from）——**已实施**（`wdp schema chart`，含相位属性全表）。

实施说明（2026-09）：`model.FieldDoc/FieldSection` 共享类型，FieldDoc 带
`GoField`（该键填充的 model.Task/model.Host 结构体字段名）——文档与结构体
机械关联，不再依赖人工两处同步：

- **task**：`taskKeys` 白名单由 `TaskFieldSections()` **派生**（字段表是
  控制键唯一清单）；`TestTaskFieldsPopulateStruct` 对每个文档键用哨兵值
  真实走 parseTask，反射断言 GoField 字段被填充（忘写解析/落点不符点名报错）；
  `TestTaskKeysFrozen` 冻结键全集，拦截"误删文档行静默收窄白名单"。
- **host**：buildHost 的字段 switch 提取为 `hostKeyApplier`（可直测单元）；
  `TestHostFieldsPopulateStruct` 同样哨兵+反射断言；基线 hostKeys ⊆ 文档在
  inventory 包对账，conn 注册键全量对账在 cli 集成测试（blank import 驱动后
  `HostKeys()` 才是全集，反向断言同时拦截 hostKeys 字面量被删）。
  `HostKeys()` 导出亦为 §1 --host-config 的前置件。
- 命令 `wdp schema [host|task|chart]` 注册于 Package 组、列在 `wdp module` 之前
  （结构骨架参考在前，区别于具体模块参数文档），总览输出同样 host/task/chart 在前
  + 指引 `wdp module`；渲染带 CJK 双宽对齐。
- `via` 中继键经组变量（inventory 组级配置）不属于主机条目键，故不在
  host 表——其语义见 `internal/inventory/via.go` 的包注释与
  [12 CLI 参考](12-cli命令参考.md#wdp-apply) 的 `--autonomous` 说明。
