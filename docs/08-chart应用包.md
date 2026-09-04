# 08 Chart 应用包

chart 是自包含的部署单元：配置（values）、模板、任务编排、生命周期（卸载/状态）打包在一个目录（或 tgz）里，借鉴 Helm 但执行语义是过程式任务。

## 目录结构

```
myapp/
├── chart.yaml          # 元数据（必需）
├── values.yaml         # 第 1 层：默认值
├── values.schema.json  # values 结构校验（可选：JSON Schema，见下文）
├── deploy.yaml         # 部署任务编排（必需，playbook 语法）
├── uninstall.yaml      # 卸载清单（可选：deploy 的逆操作）
├── status.yaml         # 只读状态探测（可选）
├── <phase>.yaml        # 任意自定义相位（可选：update/stop/download…，
│                       #   文件名即相位名，--phase <phase> 执行）
├── _helpers.tpl        # 命名模板（可选）
├── templates/          # 配置文件模板（template 模块 src 引用）
├── tasks/              # include 任务片段（可选：deploy.yaml 按需静态展开）
├── files/              # 静态文件（可选：copy src 直接分发，不经模板渲染）
├── packages/           # 制品缓存（可选：artifact 模块 cache 落位，离线安装包载体）
├── envs/               # 第 2 层：环境覆盖文件
│   ├── prod.yaml
│   └── staging.yaml
├── charts/             # 子 chart（可选，递归嵌套）
│   └── jdk/
│       ├── chart.yaml
│       ├── values.yaml
│       └── deploy.yaml # 子 chart 仅支持单个 play
└── modules/            # 自带脚本模块（可选）
    └── my-check
```

### 拆分与复用的三级工具

| 需求 | 用法 | 变量域 |
|---|---|---|
| 把大 deploy.yaml 拆文件 | `include: tasks/smart.yaml`（Load 阶段静态展开；when/tags/become 下沉到片段任务） | 共享当前域 |
| chart 内可选功能块 | include + `when: '{{ .smart }}'`（同一文件，条件开关） | 共享当前域 |
| 跨 chart 组件复用 | `chart: jdk@^1.2`（版本约束 + 引用 vars + `tasks_from` 入口相位） | Helm 作用域隔离 |
| 集群耦合配置 | 收集 play `set_fact` → 配置 play `.hostvars`（见 [05](05-playbook任务编排.md#跨主机事实set_fact--hostvars)） | 控制端 fact store |

## chart.yaml 字段

```yaml
name: myapp                     # 必需
version: 0.1.0
description: 我的应用

required:                       # 必填 values 点路径：合并后缺失任何一项立即报错
  - app.name                    # （杜绝"忘了传关键参数部署一半"）
  - db.host

marker_dir: /var/lib/wdp        # 目标机 release marker 目录（缺省此值）
no_marker: false                # true 不写 marker
check_mode: supported           # 声明脚本模块（modules/）支持 check 模式预演；
                                # 未声明时脚本模块在 --check 下被跳过

inventory_override:             # values 键白名单：允许 inventory 组/主机同名变量
  - tier                        # 反超 values（ansible 式语义的显式 opt-in）。
                                # 未列入的键同名时 values 恒赢（run/drift 预检告警）；
                                # 仅顶层 values 域生效（子 chart 作用域不适用）；
                                # lint 校验键必须是 values 顶层键；marker/drift
                                # 的 values 摘要不含覆盖结果

phases:                         # 自定义相位属性声明（可选，见"生命周期"）
  update: {release: true}       # 例：update 相位视同一次部署
  stop: {}                      # 例：普通相位（零值缺省，可省略）
```

## values schema 校验（values.schema.json）

chart.yaml 的 `required` 只回答"缺不缺"；`values.schema.json`（JSON Schema
2020-12，Helm 同名惯例）还能回答**类型对不对、取值合不合法**。文件可选，
存在时在加载期编译（语法非法立即失败），校验发生在：

- **`wdp run` 部署相位**（deploy 及声明 `release` 的相位，与 required 同门控，
  连接主机之前）；
- **`wdp lint`**：合并 values（defaults + `-f` + `--set`）+ 每个 `envs/*.yaml`
  叠加默认值后分别校验（不实际运行即可暴露某环境配置错误）+ 子 chart
  静态走查；
- **executor 子 chart 展开时**：用含引用 `vars:` 的精确作用域再校验一次。

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "app": {
      "type": "object",
      "properties": {
        "port": {"type": "integer", "minimum": 1, "maximum": 65535},
        "mode": {"enum": ["standalone", "cluster"]}
      }
    }
  }
}
```

```console
$ wdp lint ./myapp --set app.port=9090x
[ERROR] values.schema.json: myapp: values do not match values.schema.json:
- at '/app/port': got string, want integer
```

- 子 chart 各自带 schema，校验对象是其 `SubScope` 作用域（子默认 → 父
  `<子名>` 子树 → `global` → 引用 vars）——设置 `additionalProperties: false`
  的子 chart schema 需为 `global` 留出声明
- 完整示例见 `examples/docker/values.schema.json`

## 三层 values 叠加

```
values.yaml（默认） → -f 文件（可多个，按序深合并） → --set（点路径）
```

```sh
wdp run ./myapp -i prod.yaml \
  -f envs/prod.yaml \        # 第 2 层
  --set app.port=9090 \      # 第 3 层
  --set app.debug=false
```

合并规则与 `--set` 语法详见 [09-Values 与模板函数](09-values与模板函数.md)。

## 任务编排：deploy.yaml

标准 playbook 语法（见 [05](05-playbook任务编排.md)），额外获得：

- **chart 引用任务**（就地展开子 chart）
- **hook 生命周期任务**
- 模板可用 `_helpers.tpl` 命名模板与 `templates/` 文件
- chart 自带脚本模块（`modules/`）

```yaml
- name: 部署 myapp
  hosts: webservers
  become: true
  vars:
    app_dir: "{{ .global.workdir }}"
  tasks:
    - name: 首次安装初始化
      shell: './init-data.sh'
      hook: pre_install

    - name: 安装 JDK（子 chart，版本约束）
      chart: jdk@^1.0
      vars: {mirror: https://mirrors.example.com/java}

    - name: 下发配置
      template:
        src: templates/app.conf.tpl
        dest: "{{ .app_dir }}/app.conf"
      notify: 重载应用
  handlers:
    - name: 重载应用
      service: {name: myapp, state: reloaded}
```

## 子 chart：复用与作用域隔离

父 deploy.yaml 中显式引用（任务顺序即语义）：

```yaml
- name: 安装 JDK
  chart: jdk            # 展开执行 charts/jdk/deploy.yaml 的任务序列
- name: 带 版本约束
  chart: jdk@^1.2       # semver 语法：1.2 / ^1.2 / >=1.0,<2.0
- name: 引用处注入变量
  chart: jdk
  vars: {mirror: https://…}   # 优先级最高
```

**作用域隔离**（Helm 语义）——子 chart 内变量域：

```
子 chart 默认 values  →  父 values 的 jdk: 子树  →  global:（跨层共享）  →  引用 vars
```

- 子 chart **看不到**父 values 的兄弟子树（如 `app:`）——保证组件可复用
- `global:` 子树跨层共享（父与全部子 chart 可见）
- 内置变量（inventory_hostname/groups/…）与主机 facts（setup/stat）穿透子 chart 作用域
- 子 chart 的 handlers 合并进父 play（重名告警忽略；含全部相位的 handlers）；
  父 play 的 strategy/become/environment 对子任务生效
- `charts/` 支持多级嵌套

### 入口相位：tasks_from

缺省引用执行子 chart 的 `deploy.yaml`；`tasks_from` 可选其**他相位**作为入口
（值 = 相位名，即子 chart 根目录的 `<phase>.yaml`）——只复用子 chart 的某一段
（校验/备份/预下载…），等价 Ansible `include_role` 的 `tasks_from`：

```yaml
- name: 只跑 jdk 的校验
  chart: jdk@^1.2
  tasks_from: validate
```

- 作用域/handlers/父 play 属性继承与 deploy 引用完全一致
- 入口相位限单 play（多 play 相位无法内联展开，引用处报错）；未知相位报错
  并列出该子 chart 实际提供的相位
- lint 校验 `tasks_from` 相位存在性；`wdp render` 预览按入口展开

父 values 覆盖子 chart 默认值：

```yaml
# 父 values.yaml
jdk:
  version: "17"        # 覆盖 charts/jdk/values.yaml 的 version
global:
  env: prod            # 全层共享
```

## 生命周期

### 相位：文件名即相位

chart 根目录除结构文件（chart.yaml / values.yaml / inventory.yaml）外的每个
`<phase>.yaml` 都是一个生命周期相位，`--phase <名>` 执行对应清单：

```sh
wdp run ./myapp -i inv.yaml                    # --phase deploy（缺省）
wdp run ./myapp --phase status -i inv.yaml     # 只读探测
wdp run ./myapp --phase uninstall -i inv.yaml  # 卸载
wdp run ./myapp --phase update -i inv.yaml     # 自定义相位：执行 update.yaml
wdp run ./myapp --phase download -i inv.yaml   # 自定义相位：准备离线制品
```

- `deploy.yaml` 必需；`uninstall.yaml` 是 deploy 的逆操作；`status.yaml` 只读
  （marker 内容、应用产物探测）
- 自定义相位任意命名（`update.yaml`/`stop.yaml`/`download.yaml`…），未知的
  `--phase` 报错并列出 chart 实际提供的相位
- 全相位支持 `--check` / `--diff` 预演

### 相位属性：chart.yaml phases 声明

文件名决定"跑哪些任务"，相位**属性**决定语义（是否视同部署、marker 处置、
是否记部署记录）。内置缺省：

| 相位 | release | record | clears_marker | 语义 |
|---|---|---|---|---|
| `deploy` | ✔ | ✔ | - | 部署：required 校验、可逆性确认、写 marker、记部署记录 |
| `uninstall` | - | ✔ | ✔ | 卸载：清 marker、记部署记录（卸载留痕） |
| `status` | - | - | - | 只读：不记部署记录 |
| 自定义（未声明） | - | - | - | 普通相位：只跑任务 |

```yaml
phases:
  update: {release: true}   # update 视同一次部署（写 marker / 记录 / 部署前确认）
  backup: {record: true}    # 只记部署记录，不写 marker 不做部署前确认
  purge: {clears_marker: true}
```

- `release: true` 隐含 `record`；声明只能**追加**内置缺省，不能关闭
- lint 校验声明键与相位文件匹配（声明了 `bakcup:` 但没有 `bakcup.yaml` 会告警）

### hook 任务

相位文件内标记 `hook:` 的任务按相位提取，命名 `pre_<相位>` / `post_<相位>`
（deploy 相位沿用历史命名 `pre_install` / `post_install`）：

| hook | 执行时机 |
|---|---|
| `pre_install` | deploy 主任务序列之前 |
| `post_install` | deploy 全部成功（含 handlers flush）之后 |
| `pre_uninstall` / `post_uninstall` | uninstall 主任务之前 / 之后 |
| `pre_update` / `post_stop` … | 任意自定义相位的 pre/post |

- 主任务失败 → `post_*` 跳过；相位不匹配的 hook 自动跳过（uninstall 不跑 install hook）
- hook 拼写错误（`pre_stopp`）lint 告警；语法非法（不满足 `pre_/post_ + 相位名`）加载期报错
- hook 用 `run_once: true` + `delegate_to: localhost` 做"全局只执行一次"的初始化/通知

### release marker

deploy（及声明 `release` 的相位）成功后每台主机写入：

```
<marker_dir>/<chart名>/release.json
```

内容：chart 名 / 版本 / 产生本次 release 的相位 / values 摘要（sha256 前 12 位）/
部署时间 / wdp 版本。uninstall（及 `clears_marker` 相位）成功后自动清除。这是
status 相位与漂移检测的数据地基；`no_marker: true` 禁用。

### 部署前可逆性确认

自动统计任务可逆性（可逆/只读/不可逆计数与明细），存在不可逆操作时交互确认（`--yes` 跳过，非 TTY 警告放行）。详见[策略文档](06-部署策略与自动回滚.md#可逆性评估部署前确认)。

## 命名模板 _helpers.tpl

```yaml
{{/* 定义 */}}
{{ define "app.fullname" -}}
{{ .app.name }}-{{ .global.env }}
{{- end }}

{{ define "app.labels" -}}
{{ to_yaml (dict "app" .app.name "env" .global.env) | nindent 2 }}
{{- end }}
```

- 父与全部子 chart 的 helpers 合并注册（子重名覆盖父）
- 全部渲染场景可用：任务参数、配置模板、`include` 可参与管道

## 在线 / 离线安装（artifact cache-first）

`artifact` 模块让同一份任务同时服务在线与离线环境（详见 [07 模块手册](07-内置模块手册.md#artifact)）：
控制机 `packages/` 已有制品则零联网（离线），未命中则下载一次落缓存再分发（在线）——
不再需要 online/offline 两套任务和 when 开关。

推荐结构：

```
myapp/
├── deploy.yaml          # artifact 任务：url + cache + members（可选）
├── download.yaml        # 相位：仅准备 packages/ 制品（控制机执行，可离线跑）
├── tasks/download.yaml  # 下载片段（deploy 随部署预下载 或 --phase download 单独触发）
└── packages/<arch>/     # 制品缓存：原始压缩包 / 二进制（wdp package 一并打进 tgz）
```

```sh
wdp run ./myapp -i inv.yaml --phase download   # 有网环境：预下载制品到 packages/
wdp package ./myapp                            # 打包（packages/ 制品随包携带）
wdp run ./myapp-0.1.0.tgz -i inv.yaml          # 离线环境：cache 命中，全程零联网
```

压缩包类制品用 `members` 按文件名检索（kubernetes server 包、etcd 包只要
部分二进制），未命中的成员名直接失败，离线包缺文件在任务期即暴露。

**零依赖替代**（不用 artifact 的场合）：include 上的 `when` 会下沉到片段内
全部任务，两支片段都进任务树（lint/预演可见），运行时仅一支生效：

```yaml
- include: tasks/online.yaml
  when: '{{ .online_install }}'
- include: tasks/offline.yaml
  when: '{{ not .online_install }}'
```

## 打包与分发

```sh
wdp lint ./myapp                        # 结构/模块/引用/模板可渲染校验（含 block 递归、脚本模块）
wdp package ./myapp -o .                # 打包 myapp-0.1.0.tgz
wdp run ./myapp-0.1.0.tgz -i inv.yaml   # tgz 直接部署（解包有路径穿越防御）
```

## 部署记录（release）

每次 run 落一条记录到 `~/.wdp/releases/<id>.json`（chart/版本/values 快照/主机/统计）：

```sh
wdp release list                 # 全部记录（新在前）
wdp release list myapp           # 按 chart 名前缀过滤
wdp release show <id>            # 详情
wdp release show <id> --values   # 输出 values 快照 YAML，可直接 -f 重放
wdp release diff <id1> <id2>     # 对比两次部署的 values（升级前看会改哪些参数）
```

```sh
# 重放某次部署的完整参数
wdp run ./myapp -i inv.yaml -f <(wdp release show myapp-1712345678 --values)
```
