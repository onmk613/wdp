# 12 CLI 命令参考

## 全局 flag（所有子命令继承）

| flag | 默认 | 说明 |
|---|---|---|
| `--config` | ./wdp.cfg | 配置文件（显式指定时必须存在） |
| `--inventory / -i` | inventory.yaml | inventory 文件，**可多次指定**（后者覆盖合并） |
| `--forks` | 5 | 并发主机数 |
| `--timeout` | 0 | 全局墙钟超时秒（0 不限） |
| `--task-timeout` | 0 | 任务默认超时秒（任务级 timeout 属性可覆盖） |
| `--verbose / -v` | 0 | 可重复计数：-v 逐主机 / -vv 全量+loop 逐项 / -vvv 调试 |
| `--quiet / -q` | false | 仅异常与 RECAP |
| `--no-color` | false | 禁用颜色 |
| `--output` | console | console / json（机器可读） |
| `--max-download-mb` | 0 | get_url 下载响应体上限 MiB（0 = 跟随 wdp.cfg `[transfer]`，默认 2048） |

未显式指定的 flag 回退 wdp.cfg（见[配置文件](02-配置文件.md)）。

## 命令分组（`wdp --help` 分类展示）

| 分组 | 命令 |
|---|---|
| 部署命令 | `run`、`adhoc` |
| 应用包命令 | `schema`、`module`、`render`、`lint`、`package` |
| 安全与信任命令 | `ca`、`scan-ssh` |
| 代理命令 | `agent` |
| 运维与记录命令 | `release`、`drift`、`inventory` |
| 计划与自治命令 | `plan`、`apply`（见 [15 自治执行](15-自治执行改造方案.md)） |

版本信息用框架自带的 `wdp --version`。

## wdp run

```
wdp run <playbook.yaml | chart目录 | chart.tgz>
        [-i inventory（可多次）] [-f values 文件（可多次）] [--set k=v（可多次）]
        [--limit 模式] [-t/--tags 逗号分隔] [--skip-tags 逗号分隔]
        [--check] [--diff] [--list-hosts] [--start-at-task 任务名]
        [--phase <相位>] [--fact-cache facts.json] [-y/--yes]
```

| flag | 说明 |
|---|---|
| `-f / --values-file` | chart values 覆盖文件，按序深合并（同 Helm） |
| `--set` | 点路径覆盖（`--set a.b[0]=v`；`k=null` 删键） |
| `--limit` | 在 play hosts 基础上进一步收窄主机（选择模式语法） |
| `--hosts` | 内联主机表达式，免 inventory 文件：IP/`host:port`/区间 `10.8.2.101-104`（跨度上限 256）。内联清单只有 all 组——全部 play 作用于全部内联主机；与显式 `-i` 互斥 |
| `-t / --tags` | 仅执行带这些 tag 的任务 |
| `--skip-tags` | 跳过带这些 tag 的任务 |
| `--check` | 预演模式（全相位可用） |
| `--diff` | 内容级差异（自动启用 check） |
| `--list-hosts` | 仅列出将执行的主机 |
| `--start-at-task` | 从指定任务开始（调试） |
| `--phase` | chart 生命周期相位（缺省 deploy）：根目录任意 `<phase>.yaml` 都是相位——内置 `uninstall` / `status`，自定义如 `update` / `download`（见 [08 生命周期](08-chart应用包.md#生命周期)）；未知相位报错并列出可用相位 |
| `--fact-cache` | fact store 持久化 JSON：启动加载、结束原子落盘（setup/set_fact 跨运行复用；损坏自动忽略） |
| `-y / --yes` | 跳过不可逆操作确认（CI 建议） |

示例：

```sh
wdp run site.yaml -i base.yaml -i prod.yaml --limit 'web*,!web1'
wdp run ./myapp -f envs/prod.yaml --set app.port=9090 --check --diff
wdp run ./myapp-0.1.0.tgz -i inv.yaml --phase uninstall -y
wdp run ./myapp -i inv.yaml --phase download    # 自定义相位：准备离线制品
```

退出码：0 成功；1 存在失败主机或错误。

## wdp plan

```
wdp plan <chart目录|chart.tgz>
        [-i inventory（可多次）] [-f values 文件] [--set k=v]
        [--phase <相位>] [--limit 模式] [--fact-cache facts.json]
        [-o plan.json]

wdp plan show <plan.json> [--host 主机名]
wdp plan diff <plan-a.json> <plan-b.json>
```

把 chart + inventory + values 离线编译为**完全解析的执行计划**（部署相位
不连接任何主机）。plan 是一等产物：跨主机信息（groups/hosts/hostvars、
values、play vars）在编译期固化为字面值；`when`/`loop`/模板渲染保留给
执行侧（可能依赖运行时 register）。同一 chart + 同一 values 两次编译产出
**逐字节相同**的 plan.json（PlanID 内容寻址）；文件被篡改后 `plan` 拒绝加载。

| flag | 说明 |
|---|---|
| `-o / --output-file` | 写出 plan.json（缺省打印到 stdout；0600 落盘） |
| `--phase` / `--limit` / `-f` / `--set` | 与 `wdp run` 同语义 |
| `--fact-cache` | 把已有 facts 冻结进计划变量域 |

非部署相位（uninstall/status 等）的 values 来自各主机 release marker
（需连接读取，与 `run` 同语义）。

```sh
wdp plan ./myapp -i inv.yaml -f envs/prod.yaml -o plan.json   # 编译（离线）
wdp plan show plan.json --host 10.20.0.11                     # 这台机器会发生什么
wdp plan diff plan-a.json plan-b.json                         # 任务级 + values 级差异
```

plan 内嵌 chart 树快照（自包含）；`packages/` 制品目录与大文件只记录
sha256 引用（`--chart-dir` 在 apply 时本地补齐，或由模块按 URL 分发）。

## wdp apply

```
wdp apply <plan.json>
        [--limit 模式] [-t/--tags] [--skip-tags] [--check] [--diff] [-y/--yes]
        [--chart-dir chart目录] [--fact-cache facts.json]
        [--autonomous] [--detach] [--resume] [--become-password-env VAR]

wdp apply status <plan.json> <run-id前缀> [--limit 模式]
```

执行 `wdp plan` 产出的已批准计划。连接元数据来自计划本身，不依赖 chart
目录或 inventory 在现场存在；与 `wdp run` 在同一 chart 上产生相同执行
结果（`run` = `plan` + `apply` 的组合）。

| flag | 说明 |
|---|---|
| `--chart-dir` | 为计划引用的大文件制品提供本地来源（离线分发） |
| `--autonomous` | 把计划分片提交给目标 agent 自治执行：agent 收到分片后本地完成收敛并落 journal，控制端可随时断开（断连容忍）；按 `via` 中继根分组提交，跳板机 agent 即该网段本地控制端。仅支持 agent 通道（其他通道显式报错）；旧版 agent 无 `/plan` 端点时自动回退控制端直接执行 |
| `--detach` | 配合 `--autonomous`：agent 接受即返回，事后用 `apply status` 回查 |
| `--resume` | 配合 `--autonomous`：从各 agent 的 journal 断点续跑（已 ok/changed 的任务不重做；plan 变更则拒绝续跑） |
| `--become-password-env` | become 密码的环境变量（随分片下发、仅驻留 agent 内存；推荐免密 sudo） |

```sh
wdp apply plan.json -y                          # 控制端直接执行
wdp apply plan.json --autonomous -y             # 提交 agent 自治执行并等待终态
wdp apply plan.json --autonomous --detach -y    # 提交即返回
wdp apply status plan.json <run-id前缀>         # 回查自治执行进度（journal）
```

## wdp adhoc

```
wdp adhoc -m <模块名> -a '<参数>' [--become] [--format '模板'] [--check] [--diff] <主机模式>
wdp adhoc -m <模块名> -a '<参数>' --hosts <内联主机表达式>...
```

| flag | 说明 |
|---|---|
| `-m / --module` | 模块名（缺省 shell；`wdp module` 查看列表） |
| `-a / --args` | free-form 命令或 `k=v` 参数列表 |
| `-b / --become` | 提权执行 |
| `--hosts` | 内联主机表达式（IP/主机[:端口]，支持 `10.8.2.101-104` 区间），免 inventory 文件；此时主机模式可省略（默认 all），与显式 `-i` 互斥（同 `run --hosts` 口径） |
| `--format` | 逐主机模板化输出（`.host .stdout .rc …`），进 shell 管道 |
| `--check` / `--diff` | 预演 / 差异 |

```sh
wdp adhoc -m shell -a 'uptime' all
wdp adhoc -m shell -a 'uptime' --hosts 10.8.2.101-104        # 临时目标，免清单
wdp adhoc -m package -a 'name=curl state=present' --become webservers
wdp adhoc -m stat -a 'path=/etc/hosts' --format '{{.host}} {{.stdout}}' web*
```

## wdp schema

```
wdp schema [host|task] [--json]
```

**YAML 结构字段速查**（写 inventory / playbook 前的骨架参考，区别于具体模块的参数文档）：

- `wdp schema host` — 主机条目可用的全部连接参数键（地址/SSH 认证/TLS/提权口令/agent 与 push 专属），未列出的键一律视为主机变量
- `wdp schema task` — 任务控制属性全表（条件/循环/重试/提权/委托/结果判定/容错块），按语义分组附可粘贴示例
- `--json` 机器可读（编辑器插件/脚本）；无参打总览

字段表与解析器同源（对账测试防漂移）；具体模块（shell/copy/...）的参数文档用 `wdp module <名>`。

## wdp module

```
wdp module [模块名]
```

无参列出全部内置模块（名 + 描述）；带模块名输出与实现同源的参数文档与示例任务片段（永不漂移）。

## wdp render

```
wdp render <chart目录|tgz> [-f values 文件（可多次）] [--set k=v（可多次）] [--hostname 名字]
```

**chart 静态渲染预览**：合并后的最终 values + 全部 templates/ 渲染结果 + 任务树（chart 引用展开一层）。`--hostname` 指定预览用 `inventory_hostname` 占位（缺省 preview-host）。不需要 inventory/主机——写包阶段离线自查渲染；执行侧预演用 `wdp run --check`（真实主机、真实连接的模块级预演），两者互补分层。

## wdp lint

```
wdp lint <chart目录|tgz|playbook.yaml> [-f values 文件] [--set k=v]
```

校验：结构完整性、helpers 可解析、任务树（block 递归、chart 引用与版本约束、模块名、chart 脚本模块）、任务模板字段 parse 校验（裸变量引用 `{{ var }}` 这类运行时才炸的写法提前拦截）、全部模板可用样例域渲染、envs 文件可解析。发现 ERROR 时退出码非零。

## wdp package

```
wdp package <chart目录> [-o/--out-dir 输出目录]
```

先加载校验再打包为 `<name>-<version>.tgz`（包内顶层 `<name>/` 前缀，可直接 `wdp run`）。

## wdp inventory

```sh
wdp inventory [主机模式] [--vars]    # 主机模式缺省 all
```

| 说明 |
|---|
| 查看清单展开结果：主机表（HOST/ADDRESS/PORT/CONN/GROUPS，分组归属含 children 展开）；`--vars` 输出每台主机**合并后的最终变量**（YAML）——排查"组变量/host_vars 层层覆盖后到底是什么"的入口 |

## wdp drift

```
wdp drift <chart目录|chart.tgz> [主机模式] [-f values] [--set k=v] [--limit 模式]
```

只读漂移巡检：逐主机读取 release marker（`<marker_dir>/<chart>/release.json`），
与当前合并 values 的摘要（与 marker 写入时同一 `ValuesDigest` 口径）对比：

| 状态 | 含义 |
|---|---|
| `OK` | marker 摘要 = 当前 values 摘要 |
| `DRIFTED` | values 已变（改配置未部署，或现场被手改） |
| `OUTDATED` | 部署的是旧 chart 版本（values 摘要一致） |
| `NOT-DEPLOYED` | 无 marker（未部署过或已卸载） |
| `UNREACHABLE` | 连接失败 |

- 存在 `DRIFTED` / `UNREACHABLE` 时退出非零（可直接进 CI 定时巡检）
- 只读不收敛：纠偏动作是 `wdp run` 幂等重部署（刻意不做自动纠偏）
- `no_marker: true` 的 chart 无法巡检（marker 是数据源）

```sh
wdp drift ./myapp -i inv.yaml                    # 全部主机
wdp drift ./myapp 'web*' -f envs/prod.yaml       # 指定组与 values 口径
```

## wdp release

```
wdp release list [chart名前缀]
wdp release show <id> [--values]
wdp release diff <id1> <id2>
wdp release del <id> [<id>...] [--prefix | --regex] [--yes]
```

| 子命令 | 说明 |
|---|---|
| `list` | 部署记录列表（新在前，可按 chart 名前缀过滤） |
| `show` | 记录详情；`--values` 仅输出 values 快照 YAML（可直接 `-f` 重放） |
| `diff` | 两次部署 values 逐路径对比 |
| `del` | 删除记录（可多条）。默认按 ID 删、支持唯一前缀，歧义时报候选、先全解析后删；`--prefix`/`--regex` 批量删全部命中（正则非锚定），批量先列清单、`--yes` 确认执行 |

记录存于 `~/.wdp/releases/<id>.json`。

## wdp ca

```
wdp ca init   <目录> [--name <文件名>] [--days 1] [--path-len 0] [--cn <名>] [--o <组织>（可多次）] [--ou/--c/--st/--l]
              [--key-algo ed25519|ecdsa|rsa] [--key-size N]
wdp ca issue  [目录] [--name <证书名>] [--profile server|client|peer]
              --san <SAN>（可多次：IP/DNS 自动识别；uri:/email: 前缀）
              [--cn/--o/--ou/--c/--st/--l] [--key-algo/--key-size] [--days N]
              [--ca-cert <CA证书>] [--ca-key <CA私钥>]
wdp ca renew  [新证书路径] --cert <旧证书> (--key <旧私钥> | --new-key)
              [--days 30] [--ca-cert <CA证书>] [--ca-key <CA私钥>]
wdp ca show   <证书路径> [--key <私钥路径>]
```

| 子命令 / flag | 说明 |
|---|---|
| `init` | 生成**全新根 CA**（目录为位置参数；`--name/-n` 自定义产物文件名，缺省 `ca.crt/ca.key`；**默认有效期 1 天**，`--path-len` 控制可签发的中间 CA 深度（默认 0 = 只签叶子，N>0 允许 N 级，-1 不限——企业 PKI 需要中间 CA 时用）；私钥明文 0600、目录 0700——短有效期取代静态加密，见 04 文档；常驻 agent 机群用 `--days N` 放宽）。复用已有根 CA 无需导入：issue/renew 直接 `--ca-cert/--ca-key` 指向它（见下）。cfssl 风格可定制：主题（`--cn/--o/--ou/--c/--st/--l`，O 缺省 wdp）、密钥（`--key-algo ed25519（默认）|ecdsa|rsa` + `--key-size`）——产物可作任意服务的信任根 |
| `issue` | 签发证书（cfssl 风格，产物可直接交给 etcd/nginx 等任意服务）。`--name` 只决定产物文件名与 CN 缺省值（空 = 档案名 server/client/peer，`--cn` 可覆盖 CN）；**SAN 一律经 `--san`**，`--profile server\|client\|peer`（server=ServerAuth、client=ClientAuth、peer=双向，etcd mTLS 用；默认 server）；`--days` 缺省 0 = **自动档**（按签发 CA 剩余寿命分段：CA 剩余 ≤30 天 → 跟随 CA 一起到期；30~60 天 → 取剩余的 50%；>60 天 → 上限 30 天；显式给值时仍**一律钳制到 CA 到期时刻**）；`--key-algo/--key-size` 同 init；主题 flags 覆盖 O/OU/C/ST/L；输出 SHA256 指纹 |
| `--san` | 追加额外 SAN（可多次）：裸值按 IP/DNS 自动识别；`uri:spiffe://…` 与 `email:a@b.c` 前缀签 URI/Email SAN——多地址/NAT/端口转发主机一张证书覆盖全部可达地址，`renew` 续期时全量继承（含 URI/Email） |
| `renew` | **更新延期，全显式无命名约定**：`--cert` 旧证书必填；保留私钥模式 `--key` 必填（会校验与证书公钥配对，不配对拒绝），`--new-key` 换钥则无需旧钥；位置参数 = **新证书输出路径**（缺省当前目录，新私钥为同目录同名 `.key`）；身份字段（CN/SAN 四类/EKU/密钥算法）全部继承，`--days` 为在当前到期上**增加**的天数（默认 30，钳制到 CA 到期）。**新旧同目录时**旧证书/私钥先改名 `*.old.<时间戳>` 备份；输出到别处则旧件原地不动 |
| `--ca-cert/--ca-key` | 指定**根 CA（签发者）**的证书与私钥——用它给新证书**签名**，不是要生成/续期的证书本身（新证书输出到 `<输出目录>/<name>.crt\|.key`，输出目录为位置参数）。默认 `<dir>/ca.crt`/`<dir>/ca.key`；可指向任意**自制根 CA**（openssl 等，明文 SEC1 EC / PKCS8 私钥）——复用组织既有信任链就靠这对 flag，无需导入步骤。校验：必须是 CA 证书、未过期、与私钥匹配 |
| `show` | `<证书路径>` 为位置参数；`--key <私钥路径>` 校验证书与私钥是否配对（配对打印 verified，不配对错误退出非零——接手外部证书/排查错配时先验再用）。查看证书携带的信息：主题/签发者（自签标注）、序列号、有效期（剩余天数/已过期）、CA 角色与 PathLen、签名算法、公钥算法、密钥用途、扩展用途（ServerAuth/ClientAuth）、SAN（DNS/IP/URI/Email）、SHA256 指纹——接手外部根 CA、检视 push 会话证书或排查证书问题时先看清内容再信任 |

证书角色：agent 在目标机上是 TLS **服务端**（默认的 server 档案，含 SAN）；
控制端连接 agent 时是 TLS **客户端**，`--profile client` 签的就是控制端身份证书
（EKU=clientAuth），配合 agent `--pin-client-fp` 可精确吊销。

**签给其他服务**：主题/SAN/密钥/用途全部可定制，例如给 etcd 集群签双向
peer 证书（RSA 密钥 + IP SAN）：

```sh
wdp ca init ./corp-ca --days 3650 --cn corp-root --o "My Corp" --key-algo rsa --key-size 4096
wdp ca issue ./etcd --ca-cert ./corp-ca/ca.crt --ca-key ./corp-ca/ca.key \
  --name etcd1.corp --san etcd1.corp --san 10.0.0.21 --profile peer --days 365 --key-algo rsa
```


## wdp scan-ssh

```
wdp scan-ssh <主机模式> [--known-hosts 路径] [--allow-update]
```

采集 SSH 主机公钥写入 known_hosts（`host_key_check` 默认开启，新主机首次连接前执行一次）。
位置参数为主机选择模式（与 run/adhoc 同口径，从 inventory 选择），只服务于
inventory 托管的主机；`--hosts` 内联模式的主机可直接连接（host_key_check 按配置）。

| flag | 说明 |
|---|---|
| `--known-hosts` | known_hosts 路径（默认 `~/.ssh/known_hosts`） |

## wdp agent

目标机启动常驻 agent：

```
wdp agent [--listen 127.0.0.1:7602]
          [--ca <CA证书> --cert <服务端证书> --key <私钥>]
          [--pin-client-fp sha256:<指纹>（可多次）]
          [--allow-no-auth] [--cleanup-on-shutdown]
          [--systemd-unit wdp-agent]
          [--idle-timeout 0] [--max-request-mb 0]
          [--log-level info] [--log-file <路径>]
```

| flag | 说明 |
|---|---|
| `--listen` | 监听地址（默认 `127.0.0.1:<wdp.cfg [agent].port>`；对外监听需显式 `0.0.0.0:端口`）。注意：回环无认证只挡远程访问，同机其它用户仍可调用端点，多用户主机请启用 mTLS（启动时打印告警） |
| `--ca/--cert/--key` | mTLS 三件套 |
| `--pin-client-fp` | 客户端证书指纹准许名单（精确吊销：移除指纹重启即拒收） |
| `--allow-no-auth` | 显式允许无认证对外监听（仅限可信内网） |
| `--cleanup-on-shutdown` | **空闲超时退出**时自清理（push 临时 agent 用）；`/shutdown` 显式信号无论该开关与否总是自清理 |
| `--systemd-unit` | 自清理时停用的 systemd 单元名（默认 `wdp-agent`；按实际部署名指定，否则 `Restart=always` 可能在二进制删除后循环重启失败） |
| `--idle-timeout` | 超过该时长无**已认证**请求即自动退出（配合 --cleanup-on-shutdown 完成自清理）；`/health` 免认证探测与执行中的长任务不计入（0 = 永不；最小 1m；push 临时 agent 由 `wdp.cfg [agent].idle_timeout_min` 控制注入，默认 60m） |
| `--max-request-mb` | 请求体大小上限 MiB（0 = 内置默认 64；`/exec`、`/file` 上传等全部端点的请求体超过即拒绝 413） |
| `--log-level` | 日志级别 `trace\|debug\|info\|warn\|error`（默认 info）。info=执行的命令、传输的文件、解压、换证、收到停止信号等必要运行记录；debug=每次操作完整细节 + 逐请求访问日志；trace=debug 之上再对每个请求/响应做 httpdump（敏感字段遮蔽） |
| `--log-file` | 日志同时追加写入该文件（自动建父目录，0600）；**自清理不删除该文件**，退役后保留供审计；控制端可经 `agentctl logs` 或 `GET /file` 拉取 |

端点：`/exec`（命令执行）、`/file`（读写）、`/archive`（原生解压，不依赖目标机工具链）、`/health`（探活 + `cert_not_after` 证书到期时刻 + `idle_timeout_sec`/`idle_left_sec`，供批量巡检）、`/shutdown`（退出并自清理：请求体空或空 JSON `{}` 即默认清理——删自身二进制、证书材料含 CA、best-effort 停用 systemd 单元；也可 `{"systemd_unit":"...","files":[...]}` 指定单元名与额外文件/目录一并清理；无认证模式直接执行，mTLS 模式经证书校验后执行）、`/logs`（拉取近期日志：内存环形缓冲约 512KiB，`agentctl logs` 逐主机落盘即"日志传输到控制端"；完整历史走 `--log-file`）。
敏感操作（换证/远程清理/空闲自动退出）输出事件日志到 stderr（systemd 托管时进 journal，可追溯）。
安全默认：`/exec`、`/file` 提供的是目标机命令执行与文件读写原语，
**对外（非回环）监听且未配置 mTLS 时拒绝启动**（避免无意暴露 RCE）；
回环监听仅本机可访问，允许无认证（本地调试）。远程纳管的标准姿势：

```sh
ssh target 'wdp agent --listen 0.0.0.0:7602 --ca ca.crt --cert <IP>.crt --key <IP>.key &'
```

生产建议 systemd 托管（示例：`examples/wdp-agent.service`）。

## wdp agentctl

控制端运维常驻 agent（主机来源与 adhoc 一致：inventory + 主机模式；仅处理 `conn: agent` 的主机）：

```sh
wdp agentctl status all                     # 批量巡检：证书到期/剩余天数（<7 天标注需换证）、空闲剩余、版本
wdp agentctl retire db1 --yes               # 远程退役：删二进制与证书材料（含 CA）、停用 systemd 单元
wdp agentctl retire db1 --systemd-unit my-agent --file /etc/wdp-agent.conf --yes  # 指定单元名与额外清理路径
wdp agentctl logs all --out ./agent-logs        # 拉取各 agent 近期日志，逐主机写 <host>.log
```

| 子命令 | 说明 |
|---|---|
| `status` | 巡检 `/health`：证书到期与剩余天数（7 天内标注 `<< 需换证`）、空闲自退出剩余时间、agent 版本 |
| `install` | **经 SSH 安装常驻 agent**（retire 的对称命令）：推自身二进制 → 逐主机签发服务端证书（SAN=主机身份，`--ca-cert/--ca-key` 签发、产物在 `--cert-dir`）→ 上传证书三件套到 `--dir`（默认 /etc/wdp）→ 安装 systemd 单元并 `enable --now`（`--systemd-unit` 与 retire 一致）→ 可选 `--client-cert/--client-key` mTLS 直连验证 |
| `renew-cert` | **换证**：本地对 `<主机身份>.crt` 增量延期（`--days` 默认 +30，同目录自动备份 `*.old.<ts>`，`--new-key` 换钥）→ SSH 推送新证书 → `systemctl restart` 重启 agent → 可选验证。停机窗口 = 重启秒级；热换（CSR 模式）规划中 |
| `retire` | 远程退役自清理：`POST /shutdown` 默认清理（二进制、证书材料含 CA、systemd 单元）；`--systemd-unit` 指定单元名，`--file`（可多次）指定额外删除的文件/目录；`--yes` 跳过确认 |
| `logs` | 拉取各 agent 近期日志（`GET /logs` 内存缓冲）逐主机写入本地文件（`--out` 目录，默认 `./wdp-agent-logs/<host>.log`）；完整历史需目标机 `--log-file` 落盘 |

## 退出码约定

| 场景 | 退出码 |
|---|---|
| 全部成功 | 0 |
| 存在失败/不可达主机（run/adhoc） | 1 |
| 参数/加载错误（lint 失败、未知模块、缺 required 配置等） | 1（cobra 报错输出） |
