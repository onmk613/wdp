# wdp

Go 实现的自动化部署工具：单二进制、零运行时依赖，
支持 **SSH** / **常驻 agent** / **push 临时 agent** 三种远程通道，
Helm 风格 chart 打包与多环境精准部署。

> 📖 **完整用户文档见 [docs/](docs/README.md)**：快速开始、inventory 与主机分组、
> playbook 全语法、19 个内置模块手册、chart 应用包与生命周期、values 与模板函数、
> 输出控制、check/diff 预演、CLI 全量参考、最佳实践与 FAQ。
>
> 🧪 **可运行示例见 [examples/](examples/README.md)**：特性矩阵（19 模块/任务控制/
> 策略回滚/动态分组）、inventory 变量体系、以及代表性应用 webstack
> （金丝雀 + 健康门 + 自动回滚 + 子 chart + hook + 脚本模块的复杂配置部署全要素）。

## 设计要点

| 设计取舍 | wdp 的做法 |
|---|---|
| 控制端环境依赖 | 单静态二进制（交叉编译开箱即用） |
| 目标端依赖 | SSH 通道仅需 POSIX sh；agent 通道零依赖 |
| 模板语言复杂度 | Go text/template（sprig 白名单 + to_yaml/from_yaml；证书/密钥生成与凭据散列类函数不暴露） |
| 应用包依赖体系 | chart 子 chart 组合（values 命名空间隔离 + global 共享 + `@版本` 约束） |
| 每任务重复 SSH 握手 | 连接按主机复用；agent 直连常驻免握手 |
| 自定义模块扩展成本 | chart 自带脚本模块（`modules/<名>` 可执行文件即模块） |

## 快速开始

```sh
go build -o wdp ./cmd/wdp

# 生成的应用包开箱即用（最小骨架 / 全能力参考）
wdp template new myapp --full
wdp run ./myapp --check --diff -i inventory.yaml

# 本机演练（无需任何远端）
cat > inventory.yaml <<'YAML'
demo:
  hosts:
    local: {conn: local}
YAML
./wdp adhoc -m shell -a 'uname -a' -i inventory.yaml local
```

输出统一为英文（v1 前不引入双语；internal/i18n 包保留，待 v1 后再评估启用）。

## 连接类型与认证

inventory 主机条目通过 `conn` 选择连接类型：

| conn | 说明 | 认证 |
|---|---|---|
| `ssh`（默认） | 标准 SSH（base64 脚本传输，SFTP 优先/降级 exec 流式） | 认证链：私钥（`key_passphrase` 支持口令）→ ssh-agent → 默认密钥 → `password`/`password_env`（含 keyboard-interactive）；`host_key_check: true` 启用 known_hosts 校验；`become_password` 支持密码 sudo |
| `agent` | 常驻 HTTP(S) agent 直连；`wdp agentctl` 运维：`status` 批量巡检证书到期、`logs` 拉取日志、`retire` 远程退役自清理（删二进制/证书材料含 CA、停用 systemd 单元，可指定单元名与额外路径） | 无认证（默认仅回环）/ mTLS（`ca_file`+`cert_file`+`key_file`） |
| `push` | SSH 自举临时 agent：上传自身二进制与会话级 mTLS 三件套（会话 CA 落盘 `~/.wdp/push-ca` 与 wdp ca 同一套签发逻辑，全部主机共享，可配置轮换）→ 远端**真端口**启动（默认 7602，`agent_port` 可指定，占用自动换端口）→ 控制端 HTTPS 直连（不经 SSH 隧道，全程 TLS 加密）→ 结束自删二进制与证书（`keep_agent: true` 保留）；自举注入空闲超时（默认 60 分钟，`idle_timeout_min` 可调）兜底自清理——控制端崩溃/断网不再残留；agent 断连自动重自举自愈 | 临时 mTLS（客户端证书即凭证）；自举失败自动回退纯 SSH |
| `local` | 本机执行（演练/CI） | 无（注意：local 通道忽略 become） |

敏感值支持 `env:VAR` 引用或 `*_env` 独立键（`password_env`/`become_password_env`…），避免 inventory 明文。

### agent 认证部署（mTLS）

安全默认：`wdp agent` 仅绑定回环地址；对外监听且未配置 mTLS 时**拒绝启动**
（`--allow-no-auth` 可显式放行可信内网），避免无意暴露命令执行原语。

```sh
# mTLS 模式（自建 CA）
wdp ca init ./ca                                # CA（默认 1 天，PathLen=0；常驻机群 --days N 放宽；--name 自定义文件名）
wdp ca issue --dir ./ca --name 10.0.0.14 --san 10.0.0.14  # 服务端证书（SAN 一律显式给）
                                                # 多地址主机：--san 追加（可多次），renew 全量继承
wdp ca issue --dir ./ca --name ctl --profile client  # 控制端客户端证书（输出 SHA256 指纹）
ssh target 'wdp agent --listen 0.0.0.0:7602 --ca ca.crt --cert 10.0.0.14.crt \
     --key 10.0.0.14.key --pin-client-fp sha256:<ctl指纹> &'
```

**证书安全设计**：

- **短有效期取代静态加密**：CA 默认 1 天（远程部署的短周期信任根，泄漏窗口极小），
  私钥明文 0600 / 目录 0700，无口令加密负担；CA 设 `PathLen=0` 禁止签发中间 CA
- **短周期 + 续期**：叶子证书默认 30 天（`--days` 可调，不超 CA 到期），到期前
  `wdp ca renew <证书路径> --days N` 原地延期（增量语义、字段全继承、旧件自动备份）；`--new-key` 换钥即吊销旧证书；
  现阶段常驻 agent 换证 = `ca issue` 重签 + scp + 重启（远程热换方案规划中），
  `/health` 的 `cert_not_after` 字段可批量巡检剩余有效期
- **指纹准许名单（精确吊销）**：agent `--pin-client-fp`（可多次）只接受名单内指纹的
  客户端证书——撤权 = 删指纹重启 agent，无需换 CA 重发全部证书；
- **信任系统证书池**：控制端不配 `ca_file` 时走 HTTPS + 系统池（agent 用公网 CA
  证书如 Let's Encrypt 的场景）；`insecure_skip_verify: true` 为明确声明的降级

## Inventory：分组与选择

```yaml
all:
  vars: {ssh_user: root}
webservers:
  hosts:
    web1: {host: 10.0.0.11, key_passphrase_env: KEY_PW, host_key_check: true}
    web2: {conn: push, host: 10.0.0.12, user: root}
production:
  children: [webservers]
```

- **多文件合并**：`-i` 可重复指定（`-i base.yaml -i prod.yaml`），
  同名组合并（主机参数后者覆盖、组变量深合并、children 并集）
- **目录约定变量**：inventory 同目录 `group_vars/<组>.yaml`、`host_vars/<主机>.yaml`
  自动加载（`all.yaml` 合入全局）
- **主机选择语法**：`all` / 组名 / 主机名；逗号联合 `web1,web2`；`!` 排除
  `all,!web1`；`:&` 交集 `webservers:&production`（可链式）；通配 `web*`、`db?`
  （同时匹配主机名与组名）
- **内联主机（免 inventory 文件）**：`wdp run ./myapp --hosts '10.8.2.101-104'`
  直接以 IP 表达式为目标（`host`、`host:port`、IPv4 尾段区间
  `10.8.2.101-104` / `10.8.2.101-10.8.2.104`，跨度上限 256；与显式 `-i`
  互斥）。内联清单只有 `all` 组——**全部 play 一律作用于全部内联主机**
  （play 的 `hosts: 组名` 被忽略；多组编排请用 inventory 文件）。首次连接
  仍需 `wdp scan-ssh --from-hosts '10.8.2.101-104'` 采集指纹。生产环境建议
  仍以 inventory 为唯一事实源（release 记录、drift、组变量的载体）。
- **运行时动态分组**：`group_by` 模块按 facts 建组（见下文模块表），
  后续 play 用 `hosts: os_*` 通配引用——条件分组的正确位置是运行时而非 inventory 声明期

同一主机在多个组定义时按组名字典序确定性合并（后者覆盖连接参数，变量深合并）。

**inventory 变量覆盖 chart values 的约定（dig 模式）**：组级（`vars:`，含 `all` 组）
与主机级变量都可用于覆盖 chart values 默认，优先级 `all.vars < 父组 < 子组 < 主机条目
< chart values`。但键名必须用**独立键**（如 `node_exporter_port`），模板里
`dig "node_exporter_port" .port .` 变量优先、values 兜底——**同名直碰时 values 恒赢**
（刻意设计：chart 是产品、values 是参数面，若 inventory 可同名覆盖，
`wdp template render` 预览（不加载 inventory）就会与实际执行漂移）。

同名碰撞有**运行前预检**：被 values 遮蔽的 inventory 变量会告警并给出修复提示
（run/drift 都会检查，只圈定本次选中的主机），不再静默失效。确需 ansible 式同名覆盖的
chart 可在 chart.yaml 显式领取白名单：

```yaml
inventory_override: [tier]   # 这些 values 键允许 inventory 组/主机同名变量反超
```

边界：仅顶层 values 域生效（子 chart 作用域不适用，play vars 仍更高）；
lint 会校验白名单键必须是 values 顶层键；release marker 与 `wdp drift` 的
values 摘要**不包含**覆盖结果（drift 口径是 chart values 意图，inventory 覆盖是站点层）。

## Playbook

声明式 YAML 编排（模块名作为任务 key）：

```yaml
- name: web 部署
  hosts: webservers
  become: true
  serial: "25%"            # 支持 5（绝对数）/"10%"（百分比）/"5,10,20"（逐批尺寸）
  vars:
    nginx_port: 8080
  tasks:
    - name: 安装 nginx
      package:
        name: nginx
        state: present

    - name: 下发配置（--diff 可预览内容级差异）
      template:
        src: nginx.conf.tpl
        dest: /etc/nginx/nginx.conf
      notify: 重载 nginx

    - name: 从 lb 摘除本机（委托执行：变量域仍是本机，结果归属本机）
      shell: 'curl -s -X POST lb/api/remove?host={{ .inventory_hostname }}'
      delegate_to: lb01

    - name: 初始化集群（整批只跑一次，结果复制到全部主机）
      shell: './cluster-init.sh'
      run_once: true

    - name: 嵌套循环（内层 loop 用自定义变量名）
      shell: 'echo {{ .outer }}-{{ .inner }}'
      loop: [a, b]
      loop_control: {loop_var: outer}
      # 内层任务再用 loop_control.loop_var: inner

    - name: 噪音任务只看尾部
      shell: './build.sh'
      output: tail=30        # 展示控制：full/none/oneline/head=N/tail=N；no_log: true 等价 none

    - name: 等待服务就绪（until 轮询，.result 引用本轮结果）
      shell: 'curl -sf http://localhost:{{ .nginx_port }}/health'
      until: '{{ if eq .result.rc 0 }}ok{{ end }}'
      retries: 10       # 重试次数：总尝试 = retries+1
      delay: 3
      timeout: 120

    - name: 容错部署组
      block:
        - shell: './deploy.sh'
      rescue:
        - shell: './rollback.sh'
      always:
        - shell: './notify.sh'

  handlers:
    - name: 重载 nginx
      service:
        name: nginx
        state: reloaded
```

支持的任务控制属性：`when`（模板表达式）、`loop`/`with_items`、
`loop_control.loop_var`、`register`、`notify`、`tags`、`environment`、
`ignore_errors`、`retries`/`delay`、`timeout`（任务超时秒）、`until`、
`block`/`rescue`/`always`（支持嵌套）、`become`/`become_user`、
`changed_when`/`failed_when`、`delegate_to`（委托，支持 localhost）、
`run_once`、`output`/`no_log`（展示控制）、`hook`（生命周期钩子）、
`include`（任务片段，见下）。
play 级支持 `serial`（数/百分比/列表）与 `strategy` 部署策略。

register 结果在批次间延续（第一批注册的变量第二批可用），loop 注册含 `results` 逐项数组。

### 任务片段 include（文件级拆分，共享变量域）

大 playbook 拆文件不需要子 chart（子 chart 是组件复用，有 Helm 作用域隔离）：

```yaml
tasks:
  - include: tasks/smart.yaml        # Load 阶段静态展开（import 语义）
    when: '{{ .smart }}'             # include 上的 when/tags/become 下沉到
    tags: [smart]                    # 每个片段任务（when 与片段自身条件 AND）
```

- 片段相对路径以片段自身目录解析（嵌套 include 逐级相对），深度上限 32 防环
- 静态展开：`lint`/`render`/`--check` 看到的就是完整任务树，坏路径加载期报错
- `include` 不支持 `loop`（静态展开无运行期项；循环写在片段内任务上）

### 跨主机事实：set_fact + hostvars（两段式编排）

多主机同服务耦合（etcd 初始集群、主从复制）用"先收集、后配置"：

```yaml
- name: 收集（每台主机登记自己的事实）
  hosts: etcd
  tasks:
    - set_fact:
        node_id: '{{ .inventory_hostname | trunc 2 }}'

- name: 配置（渲染时引用全集群事实）
  hosts: etcd
  tasks:
    - template:
        src: etcd.conf.tpl     # 模板内：{{ range .play_hosts }}{{ (index $.hostvars .).node_id }},{{ end }}
        dest: /etc/etcd/etcd.conf
```

边界：`register` 结果只进本机变量域；`set_fact` 同时进控制端 fact store（跨主机可见）。
`--fact-cache <path>` 可把 facts 持久化为 JSON 跨运行复用（启动加载、结束原子落盘）。
运行期扩容用 `add_host` 模块注册新主机（可入组），后续 play 的 `hosts:` 与
`groups`/`hosts`/`hostvars` 即刻可见。

### 部署策略：金丝雀 / 滚动 / 健康门 / 自动回滚

```yaml
- name: 分批发布
  hosts: webservers
  strategy:
    type: canary          # linear | rolling | canary（canary 首批 1 台金丝雀）
    batch: "10%"          # 每批主机数：百分比或绝对数（缺省 25%）
    gate:                 # 每批完成且 handlers flush 后执行健康门
      shell: 'curl -sf http://localhost:8080/health'
      retries: 10         # 门重试次数（缺省判据 rc==0）
      delay: 3
    auto_rollback: true   # 批次失败或门未过 → 回滚该批变更
  tasks: …
```

- **批次失败或健康门未通过 → 终止后续批次**，`auto_rollback` 时按变更日志逆序回滚该批主机
- 回滚覆盖文件类变更：copy/template/file 全量（新建删除 + 覆盖前快照恢复），
  unarchive 部分可回滚（删除本次新建目录；覆盖已有目录内的文件不恢复快照）；
  shell/package/service/user 等过程性变更无法自动回滚（明确提示）
- play 正常结束后自动清理目标机上的回滚快照目录（不再残留 /tmp）
- 与 chart 子任务兼容（父策略对子 chart 任务生效）

变量优先级：register/item > task vars > play vars > chart values > 主机 vars > 组 vars > all.vars。
模板语法为 Go text/template：`{{ .nginx_port }}`；未定义变量直接报错（尽早暴露拼写问题）。

## Chart：复杂配置打包与多环境精准部署（借鉴 Helm）

复杂部署不再用单个 playbook，而是打包为自包含 chart：

```
mychart/
├── chart.yaml          # name / version / description / required / marker_dir / no_marker
├── values.yaml         # 第 1 层：默认值
├── deploy.yaml         # 任务编排（过程式 playbook，可含 hook 任务）
├── uninstall.yaml      # 卸载清单（deploy 的逆操作，可选）
├── status.yaml         # 只读状态探测（可选）
├── _helpers.tpl        # 命名模板 define/include（父子 chart 合并注册）
├── templates/          # 配置文件模板（template 模块 src 引用）
├── tasks/              # include 任务片段（deploy.yaml 按需静态展开）
├── files/              # 静态文件（copy src 直接分发，不经模板渲染）
├── envs/               # 第 2 层：环境覆盖文件
│   ├── prod.yaml
│   └── staging.yaml
├── charts/             # 子 chart（组件复用，支持多级嵌套）
│   └── jdk/…
└── modules/            # chart 自带脚本模块（可选，见下文）
    └── my-check
```

### 三层 values 叠加（同 Helm 合并规则）

```sh
wdp run ./mychart -i inv.yaml \
  -f envs/prod.yaml \        # 第 2 层：覆盖文件（可多个，按序深合并）
  --set app.port=9090 \      # 第 3 层：点路径（支持 a.b[0]=v 下标）
  --set app.debug=false
```

map 递归合并；标量与列表整体替换；`--set k=null` 删除默认键。

### 子 chart：显式引用、版本约束、就地展开

wdp 的任务序列**顺序即语义**，因此子 chart 在父 deploy.yaml 中显式引用：

```yaml
- name: 安装 JDK
  chart: jdk                  # 就地展开 charts/jdk/deploy.yaml 任务序列
  chart: jdk@^1.2             # 语义化版本约束（semver 语法：1.2 / ^1.2 / >=1,<2）
  vars: {mirror: https://…}   # 引用处注入变量（优先级最高）
```

作用域隔离（Helm 语义）：子 chart 内变量域 = **子 chart 默认 values →
父 values 的 `jdk:` 子树 → `global:`（跨层共享）→ 引用 vars**，
看不到兄弟子树，保证组件可复用。

### 应用包生命周期（hook / uninstall / status / marker）

chart 可携带完整生命周期，成为一个自描述的应用包：

```yaml
# deploy.yaml 中的 hook 任务按相位提取执行：
- name: 仅首次安装执行（主任务前）
  shell: './init-data.sh'
  hook: pre_install          # pre_install | post_install | pre_uninstall | post_uninstall

- name: 部署成功后执行
  shell: './notify.sh'
  hook: post_install
```

- **hook 语义**：pre_* 在主任务序列前、post_* 在 play 全部成功（含 handlers flush）后执行；
  相位过滤自动完成（uninstall 时不跑 install hook）；hook 内 register/回滚日志与主流程贯通
- **可逆性评估与确认**：部署前自动统计任务可逆性——copy/template/file 可回滚可卸载，
  shell/package/service/user 等为不可逆；存在不可逆操作时交互终端逐次确认，
  `--yes/-y` 跳过，非 TTY 打印警告放行
- **release marker**：部署成功后每台主机写入 `<marker_dir>/<chart>/release.json`
  （chart 名/版本/values 摘要/时间，缺省 `/var/lib/wdp`，`marker_dir` 覆盖、`no_marker: true` 禁用）；
  uninstall 成功后自动清除——status 与后续漂移检测的数据地基
- **required 配置项**：`chart.yaml` 声明 `required: [app.port, db.host]`，
  合并后 values 缺失任何一项立即报错

```sh
wdp run ./myapp -f envs/prod.yaml                  # 部署（--phase deploy 缺省）
wdp run ./myapp --phase status                      # 状态探测（只读）
wdp run ./myapp --phase uninstall                   # 卸载并清除 marker
wdp run ./myapp --check --diff                      # 全相位支持零风险预演 + 内容级差异
```

### 生成应用包：wdp template new

```sh
wdp template new myapp                     # 最小骨架：直接填写即可用
wdp template new myapp --full              # 全能力参考：策略/hook/委托/动态分组/子 chart/全部新模块均有示例
wdp template module user             # 输出任意内置模块的参数文档与示例任务片段
wdp template module user                  # 同上（模块详情）
```

生成物是**质量门控**的：`wdp lint` 零错误、`--check` 本机演练通过
（见 `internal/skel/skel_test.go`，随 CI 回归）——保证"直接填写使用"开箱即绿。

### chart 自带脚本模块

内置注册表未命中时，wdp 按目录约定发现 chart 的自有模块：

```
modules/
└── my-check          # 任意可执行脚本（sh/bash/python…，目标机需有解释器）
```

```yaml
- name: 自检
  my-check: {timeout: 5}
```

契约：参数经环境变量注入（`WDP_MODULE_ARGS` JSON 对象 / `WDP_FREE_FORM` 原文）。
check 模式下脚本模块默认**跳过**（脚本是外部代码，`--check` 不保证预演安全）；
chart.yaml 显式声明 `check_mode: supported` 后才会执行并注入 `WDP_CHECK=1`，
由脚本自行返回只读预演结果。stdout 输出
`{"changed":..,"failed":..,"msg":".."}` 精确判定，缺省 rc==0 即变更。
应用包无需改 wdp 源码即可扩展模块，且 SSH/agent 通道行为一致。

### 强制内置变量（所有任务模板可直接引用）

执行引擎向每台主机强制注入（不可被 play vars/chart values 覆盖，子 chart 作用域同样可用）：

| 变量 | 内容 |
|---|---|
| `inventory_hostname` / `group_names` | 自己是谁 / 自己所属组 |
| `play_hosts` / `play_batch` | 本次 play 全部选中主机 / 当前批次主机（名字列表） |
| `groups` | `map 组名→成员主机名[]`（含 children 展开，`all`=全部；group_by/add_host 动态组同样进入） |
| `hosts` | `map 主机名→{name,address,port,conn}`：`{{ (index .hosts "web2").address }}` 拿同伴地址 |
| `hostvars` | `map 主机名→该主机变量域`（inventory 变量 + fact store）：`{{ (index .hostvars "web2").node_id }}` 读同伴事实。可选键用 `dig`：`{{ dig "k" "" (index .hostvars "web2") }}` |

### 模板函数

Go text/template 全部内建函数 + **sprig v3 全集**（Helm 同款成熟库：`dict/list/concat/ternary/
indent/nindent/randAlphaNum/regexMatch/regexReplaceAll/sha256sum/now/date/semverCompare…`）
+ wdp 自有函数（覆盖 sprig 同名者，保持旧 chart 兼容）：
`default / upper / lower / trim / quote / b64enc / b64dec / split / join / replace /
contains / hasPrefix / hasSuffix / to_json / to_yaml / from_yaml`，以及 `include`（命名模板）。

```yaml
labels: {{ to_yaml (dict "app" .app.name "env" .global.env) | nindent 2 }}
```

### 工具链

```sh
wdp template new <name> [--full|--module m]  # 生成应用包骨架 / 查看模块用法
wdp template render ./mychart -f envs/prod.yaml   # 预览合并 values + 模板渲染 + 任务树
wdp lint ./mychart                          # 结构/模块/引用/模板可渲染校验（含 block 递归、脚本模块）
wdp package ./mychart -o .                  # 打包 <name>-<version>.tgz（可直接 run）
wdp run ./mychart-0.1.0.tgz …               # tgz 直接部署
wdp release list / show <id> [--values]     # 部署记录审计；--values 输出快照可 -f 重放
wdp release diff <id1> <id2>                # 对比两次部署的 values（升级前看会改哪些参数）
```

与 Helm 的关键差异：wdp 渲染产物是「最终 values + 配置文件 + 任务清单」，
执行语义仍是过程式任务（主机部署没有 k8s 的原子 apply 语义），不做 release 回滚。

## 输出体系

- **详细级别**（可重复）：缺省聚合（仅异常主机行 + 每任务一行汇总）；`-v` 逐主机全量；
  `-vv` 完整 stdout/stderr + loop 逐项；`-vvv` 调试（stderr 恒显示）；`-q` 仅异常与 RECAP
- **任务级展示控制** `output: full|none|oneline|head=N|tail=N`：只控回显不控数据——
  register 始终拿完整 stdout，适合噪音任务；**敏感任务用 `no_log: true`**：除回显遮蔽外，
  JSON 报告中 stdout/stderr/msg/diff 一并遮蔽为 `"<redacted>"`（防敏感输出进 CI 工件）
- **loop 逐项**：`-vv` 显示每项结果与 item 标签；聚合模式下异常项始终可见
- **adhoc `--format`**：单主机一行模板化输出，进 shell 管道：
  `wdp adhoc -m shell -a 'uptime' --format '{{.host}}: {{.stdout}}' all`
- **`--output json`**：完整执行记录（plays → tasks → 逐主机结果 + loop items + diff + recap），
  进度信息不打印，适配 CI/CD 解析

## dry-run 与应用级检查

- **`--check`**：全模块预演（copy/template 校验和对比、file 属性探测、package 逐包探测、
  service 状态探测、user/group 漂移探测、systemd_unit 只读对比、脚本模块注入 WDP_CHECK=1）；
  **全相位可用**（deploy/uninstall/status）
- **`--diff`**（自动启用 check）：内容级差异——copy/template 输出 unified diff（红绿着色），
  file/user/group/service 展示属性 before→after，package 列出逐包增删；JSON 模式含结构化 `diff` 字段
- **`wdp release diff`**：两次部署的 values 逐路径对比（不碰主机）
- check 模式 RECAP 明确标注 changed 为预估计数

## 内置模块

| 模块 | 说明 |
|---|---|
| `shell` / `command` | 远端执行（`creates`/`removes`/`chdir` 幂等保护） |
| `script` | 上传并执行本地脚本 |
| `copy` | 分发文件或 `content` 内容，sha256 幂等，`backup`/`mode`/`owner` |
| `fetch` | 拉取远端文件（`flat` 或 `dest/主机/路径` 层级） |
| `file` | state: file/directory/link/touch/absent + mode/owner/group |
| `template` | 本地渲染 Go 模板后分发（幂等同 copy） |
| `package` | 自动识别 apt/dnf/yum/apk/zypper（读 /etc/os-release） |
| `service` | systemd 状态与自启管理 |
| `setup` | 采集 facts（os/hostname/cpus/memory/disk/default_ipv4/euid），自动并入变量域 |
| `set_fact` | 键值对写入本机变量域 + 控制端 fact store（跨主机经 `.hostvars` 可见） |
| `add_host` | 运行期注册/更新主机（可入组），后续 play 的 hosts 选择即刻可用 |
| `user` / `group` | 系统用户/组管理（探测漂移才变更，需 become） |
| `systemd_unit` | 下发 .service 单元 + daemon-reload + enable/状态管理（复用幂等推送） |
| `unarchive` | 本地/远端 tar(.gz/.xz)/zip 解包（`creates` 幂等守卫） |
| `get_url` | URL 下载到远端（sha256 校验幂等，支持 headers/timeout） |
| `lineinfile` | 行级配置管理（regexp 匹配替换/删除、insertafter、create） |
| `wait_for` | 等待 TCP 端口/文件就绪（控制端视角，超时/间隔可配） |
| `stat` | 远端文件元数据 → register（exists/mode/size/owner/checksum） |
| `group_by` | 按 facts/变量值动态建组（配合后续 play 的通配 hosts 选择） |
| （脚本模块） | chart `modules/` 目录自带可执行模块，参数走 WDP_* 环境变量 |

`wdp template module` 列出全部；`wdp template module <名>`（或 `wdp template module <名>`）输出参数文档与示例
——文档来自模块自描述（`UsageProvider` 接口），与实现同源不会漂移。

模块在**控制端**实现、仅依赖 exec/upload/download 三原语，
因此 SSH 与 agent 通道行为完全一致；`--check` 模式下各模块返回真实变更预估而不实际变更。

## CLI

```
wdp run <playbook|chart目录|tgz> [-i inventory（可多次）] [-f values] [--set k=v]
       [--hosts 内联主机（IP/区间，免 inventory 文件）]
       [--limit 模式] [-t tags] [--check] [--diff] [--list-hosts] [--start-at-task 任务]
       [--phase deploy|uninstall|status] [--fact-cache facts.json] [-y]
wdp adhoc -m shell -a 'uptime' [--format '{{.stdout}}'] [--check|--diff] <主机模式>
wdp drift <chart目录|tgz> [主机模式]    # 只读漂移巡检：marker values 摘要 vs 当前合并 values
wdp template new <name> [--full] [--module m] [--dir d]
wdp template render / lint / package <chart>
wdp ca init / issue / renew  # mTLS 证书工具（加密 CA / 短周期 / 续期）
wdp scan-ssh <主机模式>            # 采集 SSH 主机指纹到 known_hosts
wdp release list / show / diff
wdp agent [--ca/--cert/--key] [--pin-client-fp <指纹>]
wdp template module [模块名]          # 模块列表/详情（版本信息：wdp --version）
```

`wdp drift` 逐主机比对 release marker 与当前 values 摘要，状态为
`OK / DRIFTED / OUTDATED / NOT-DEPLOYED / UNREACHABLE`；存在 DRIFTED/UNREACHABLE
时退出非零（可直接进 CI）。只读不收敛——纠偏动作是 `wdp run` 幂等重部署。

全局 flag（所有子命令继承）：`--config`、`--inventory/-i`（可多次）、`--forks`、
`--timeout`（全局墙钟）、`--task-timeout`（任务默认超时）、
`--verbose/-v`（可重复计数）、`--quiet/-q`、`--no-color`、`--output console|json`。

## 配置文件 wdp.cfg

TOML 格式，默认读取当前目录 `wdp.cfg`（不存在则静默跳过，全部用内置默认），
`--config` 可显式指定（此时文件必须存在）。优先级：**CLI flag 显式值 > wdp.cfg > 内置默认**。

```toml
[inventory]
path = "inventory.yaml"      # -i 的默认值

[run]
forks = 20                   # 并发主机数
timeout = 0                  # 全局墙钟超时（秒），0 不限
task_timeout = 300           # 任务默认超时（秒），0 不限
verbose = false              # true 等价 -v

[output]
color = true                 # 颜色输出

[ssh]                        # inventory 主机条目未显式指定时的连接默认值
user = "root"
connect_timeout = 10
host_key_check = true          # 安全默认；新主机先 wdp scan-ssh 采集指纹
known_hosts = ""             # 空 = ~/.ssh/known_hosts

[agent]
port = 7602                  # agent/push 默认端口
idle_timeout_min = 60        # push 临时 agent 空闲自清理分钟（0 = 默认 60；<0 禁用）；
                             # 控制端崩溃/断网时远端 agent 依赖该周期兜底自删
```

## 超时与大规模主机

- **三级超时**：连接级（`connect_timeout`，默认 10s）→ 任务级（`timeout` 属性或
  `--task-timeout`）→ 全局（`--timeout`）
- **并发建连限流**（`2×forks`），防千台主机瞬时 SSH 握手洪峰
- **聚合输出**（默认）：每任务仅逐行打印 failed/unreachable + 一行汇总；
  RECAP 超 100 台折叠为 TOTAL；`-v` 切回全量
- 单任务输出超 1MB 自动截断（防 cat 大文件内存爆）
- 压测：`scripts/gen-inventory.sh 1000` + `examples/bench`，
  1000 台 × 5 任务实测约 5 秒

## 架构

```
cmd/wdp                 入口
internal/
  model/                Host / Play / Task / TaskResult / Stats
  inventory/            YAML 清单：组、children、变量合并、多文件合并、
                        group_vars/host_vars、模式选择（联合/交集/通配）、动态组
  playbook/             声明式任务编排解析（block/rescue/always/until/hook）
  chart/                chart 加载/values 深合并/lint/package/版本约束子 chart 解析
  render/               Go text/template 引擎（sprig 白名单 + 自有函数 + helpers 命名模板）
  i18n/                 中英双语运行时文案（T(en, zh)，按 locale 自动选择）
  config/               wdp.cfg 全局默认配置（组合根显式注入各层，无隐式全局读）
  fmtutil/              终端表格/颜色/打印辅助
  shellquote/           shell 词法切分与单引号转义（全项目唯一注入防线）
  executor/             编排引擎：forks 并发、when/loop/register/notify/handlers、
                        chart 展开、block 容错、until 轮询、hook、delegate_to/run_once、
                        跨批次变量延续、策略分批/健康门/自动回滚
  module/               模块注册表 + 内置模块（check/diff/回滚感知）+ 脚本模块机制
  connection/           Connection 接口 + Manager（建连限流）
    sshconn/            SSH（认证链/known_hosts/密码 sudo/SFTP）
    agentconn/          HTTP(S) agent 客户端（mTLS）
    push/              临时 agent 自举（临时 mTLS + 证书轮换 + 自清理 + 回退）
    localconn/          本机执行
  agent/                agent 服务端（mTLS/shutdown/自清理）
  ca/                   自建 CA 与证书签发
  release/              部署记录
  report/               控制台输出（级别体系/展示控制/diff 着色）+ JSON + 格式化
  skel/                 wdp template new 应用包生成器（内嵌骨架 + 质量门测试）
  cli/                  cobra 命令树
```

## 已知限制与路线图

- push 临时 agent 要求目标机与控制端二进制平台一致（或 `binary_path` 指定预编译产物）
- become 密码在未启用 mTLS 的常驻 agent 通道经请求体明文传输（对外纳管建议 mTLS，
  连接层会在明文 http + become 密码时打印一次性告警）；push 通道全程临时 mTLS 加密
- `local` 通道忽略 become（本机执行不提权，首次遇到 become 任务时打印一次性告警）
- **agent 回环无认证只挡远程访问**：同机其它本地用户仍可调用 `/exec` `/file`
  （多用户主机务必启用 mTLS；启动时对无认证回环监听打印告警）
- `mode` 参数仅支持权限位（0..0777）：setuid/setgid/sticky（4755 等）会被显式拒绝
  （全链路会静默丢弃特殊位产生错误权限），需要特殊位时用 shell 模块 `chmod 4755`
- `host_key_check` 默认开启：新主机首次连接前需 `wdp scan-ssh <模式>` 采集指纹
  （明确接受风险时可 `host_key_check: false` 关闭）
- 无 CRL/OCSP：证书吊销依赖指纹名单（`--pin-client-fp`）与短周期轮换（`ca renew --new-key`）
- 自动回滚覆盖文件类变更：copy/template/file 全量，unarchive 部分（仅删除新建目录，
  覆盖已有文件不恢复）；shell/package/service/user 的过程性变更无法自动回滚；
  delegate_to 产生的变更按实际执行主机回滚
- `wait_for` 的端口探测为控制端视角（目标机本地防火墙视角可能不同；需要目标机视角时用 shell + until）
- `package` 的 `state: latest` 无法精确判定"是否真的升级"，视为 changed
- `--set` 暂不支持多级下标（`a[0][1]`）
- 漂移检测为"检测 + 报告 + 人工收敛"（`wdp drift` → `wdp run`），**刻意不做自动纠偏**：
  SSH 推送模型上的自动 reconcile 是事故放大器，持续收敛属于 Salt/K8s 控制器范式
- hostvars 快照按批次生成：同批次内先执行主机写入的 facts 对后执行主机不可见
  （下一批次/play 可见）——跨主机共享请在独立收集 play 中 set_fact
- 规划：agent 常驻定时漂移上报（复用 `wdp drift` 判定逻辑）、内容寻址分发缓存、
  远程 chart 仓库 + chart 签名（复用 CA）、stdout 流式回传、
  真机集成测试矩阵（SSH 认证链×sudo×mTLS×push 自举）

## 开发

```sh
go build ./... && go test -race ./...
```

测试基于 `connection.Fake`（内存假连接，尊重 ctx 取消）与 httptest，不依赖真实主机；
`internal/skel` 的生成物测试（lint + check 演练）构成 `wdp template new` 的质量门。

CI（`.github/workflows/ci.yml`）门禁：build / vet / gofmt / `go mod tidy` 一致性 /
`go test -race` / golangci-lint，另含 govulncheck 已知漏洞扫描；
本地 lint 用 `golangci-lint run`（配置见 `.golangci.yml`）。

## 许可证

MIT License，详见 [LICENSE](LICENSE)。
