# 03 Inventory 主机清单

inventory 描述"有哪些主机、怎么连、属于哪些组、带什么变量"。

## 基本结构（YAML）

```yaml
all:                          # 全局组：变量对全部主机生效
  vars:
    ssh_user: root
    ntp_server: ntp.example.com

webservers:                   # 组
  hosts:                      # 组内主机：名字 → 连接参数与变量
    web1: {host: 10.0.0.11}
    web2: {host: 10.0.0.12, port: 2222}
  vars:                       # 组变量（作用于组内全部主机）
    nginx_port: 8080

dbservers:
  hosts:
    db1: {host: 10.0.1.10}

production:                   # 组的嵌套：children 引用其它组
  children: [webservers, dbservers]
  vars:
    global.env: prod          # 注意：组变量是普通 map，点号键不会展开（见 values 章节）
```

规则：

- 每个主机条目中，**连接参数键**（见下表）被提取，**其余键全部成为主机变量**
- `all.vars` 是最低优先级变量层
- children 递归展开，引用不存在的组会在加载时报错
- **同一主机出现在多个组**：按组名字典序确定性合并，后处理的组覆盖连接参数（变量深合并）——不再报错，适合"基础清单 + 环境清单"分层维护

## 主机条目连接参数全表

| 键 | 类型 | 说明 |
|---|---|---|
| `conn` | string | 连接类型：`ssh`（默认）/ `agent` / `push` / `local` |
| `host` | string | 实际地址（缺省等于主机名） |
| `port` | int | SSH 端口，缺省 22 |
| `user` | string | SSH 用户（缺省取 wdp.cfg `[ssh].user`） |
| `password` / `password_env` | string | SSH 密码认证（含 keyboard-interactive） |
| `key_path` | string | 私钥路径（缺省依次尝试 ~/.ssh/id_ed25519、id_rsa） |
| `key_passphrase` / `key_passphrase_env` | string | 私钥口令 |
| `host_key_check` | bool | 校验主机指纹（缺省取 wdp.cfg） |
| `known_hosts` | string | 指纹文件路径（缺省 ~/.ssh/known_hosts） |
| `connect_timeout` | int | 建连超时秒 |
| `agent_url` | string | conn=agent 时的服务地址，如 `http://10.0.0.5:7602` |
| `agent_port` | int | agent/push 端口（无 agent_url 时用 host:port） |
| `token` / `token_env` | string | agent token 认证 |
| `tls` | bool | agent 通道启用 HTTPS |
| `ca_file` / `cert_file` / `key_file` | string | agent 通道 mTLS 三件套 |
| `insecure_skip_verify` | bool | 跳过服务端证书校验（明确声明的降级） |
| `binary_path` | string | push 通道自举用的 wdp 二进制（跨平台场景） |
| `keep_agent` | bool | push 通道结束后保留临时 agent（调试） |
| `become_password` / `become_password_env` | string | sudo 密码（缺省免密 sudo） |

**其余键 = 主机变量**。例如 `web1: {host: 10.0.0.11, role: primary}` 中 `role` 可在模板里用 `{{ .role }}` 引用。

## 变量合并顺序（低 → 高）

```
all.vars  <  父组 vars  <  子组 vars  <  主机自身 vars
```

同一主机的组归属链按"父组先、子组后"叠加。此外执行期还强制注入内置变量（不可覆盖，见 [Playbook](05-playbook任务编排.md#内置变量)）。

## 目录约定：group_vars / host_vars

inventory 文件**同目录**下的两个目录自动加载：

```
inventories/
├── prod.yaml
├── group_vars/
│   ├── all.yaml          # 合入全局变量
│   ├── webservers.yaml   # 合入 webservers 组变量
│   └── dbservers.yaml
└── host_vars/
    └── web1.yaml         # 合入 web1 主机变量
```

- 变量深合并（嵌套 map 递归），文件层覆盖 YAML 内联 vars
- 不存在的组/主机的文件被忽略（组可能在别的 inventory 文件中定义）

## 多文件合并：-i 可重复

```sh
wdp run site.yaml -i base.yaml -i prod.yaml
```

合并语义（按命令行顺序，后者覆盖前者）：

- 同名组：主机参数覆盖合并、组变量深合并、children 取并集
- 各文件同目录的 `group_vars/`/`host_vars/` 均按序加载
- 典型用法：`base.yaml` 管主机与分组，`prod.yaml` 只写环境差异

## 主机选择模式（hosts / --limit / adhoc 参数）

选择器支持以下语法（逗号分隔联合、`!` 排除、`:&` 交集、通配）：

| 表达式 | 含义 |
|---|---|
| `all` 或 `*` | 全部主机 |
| `webservers` | 组名（含 children 递归展开） |
| `web1` | 精确主机名 |
| `web1,db1` | 逗号联合 |
| `all,!web1` | 排除 web1 |
| `webservers:&production` | 交集：既在 webservers 又在 production（可链式 `a:&b:&c`） |
| `web*` | 通配：同时匹配主机名与组名（组名命中则展开为成员） |
| `db?` | `?` 单字符通配 |

```yaml
- hosts: "webservers:&production,!canary"   # 生产 web 且非金丝雀机
```

未知的主机/组名会直接报错（防拼写错误静默空跑）。

## 运行时动态分组：group_by

inventory 声明期没有主机 facts，按系统属性分组的正确位置是**运行时**：

```yaml
- name: 采集并分组
  hosts: all
  tasks:
    - setup:                        # 采集 facts（os.family 等）
    - group_by: 'os_{{ .os.family }}'   # 动态建组 os_debian / os_redhat …

- name: 家族差异化部署
  hosts: "os_*"                     # 后续 play 用通配引用动态组
  tasks: …
```

动态组同样进入内置变量 `.groups`，批次边界处生效。详见 [group_by 模块](07-内置模块手册.md#group_by)。
