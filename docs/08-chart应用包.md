# 08 Chart 应用包

chart 是自包含的部署单元：配置（values）、模板、任务编排、生命周期（卸载/状态）打包在一个目录（或 tgz）里，借鉴 Helm 但执行语义是过程式任务。

## 目录结构

```
myapp/
├── chart.yaml          # 元数据（必需）
├── values.yaml         # 第 1 层：默认值
├── deploy.yaml         # 部署任务编排（必需，playbook 语法）
├── uninstall.yaml      # 卸载清单（可选：deploy 的逆操作）
├── status.yaml         # 只读状态探测（可选）
├── _helpers.tpl        # 命名模板（可选）
├── templates/          # 配置文件模板（template 模块 src 引用）
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
```

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
- 子 chart 的 handlers 合并进父 play（重名告警忽略）；父 play 的 strategy/become/environment 对子任务生效
- `charts/` 支持多级嵌套

父 values 覆盖子 chart 默认值：

```yaml
# 父 values.yaml
jdk:
  version: "17"        # 覆盖 charts/jdk/values.yaml 的 version
global:
  env: prod            # 全层共享
```

## 生命周期

### 三个相位

```sh
wdp run ./myapp -i inv.yaml                    # --phase deploy（缺省）
wdp run ./myapp --phase status -i inv.yaml     # 只读探测
wdp run ./myapp --phase uninstall -i inv.yaml  # 卸载
```

- `uninstall.yaml` 是 deploy 的逆操作清单（未提供则该相位报错）
- `status.yaml` 只读：marker 内容、应用产物探测（`stat`/`shell`）
- 全相位支持 `--check` / `--diff` 预演

### hook 任务

deploy.yaml 内标记 `hook:` 的任务按相位提取：

| hook | 执行时机 |
|---|---|
| `pre_install` | deploy 主任务序列之前 |
| `post_install` | deploy 全部成功（含 handlers flush）之后 |
| `pre_uninstall` | uninstall 主任务之前 |
| `post_uninstall` | uninstall 主任务之后 |

- 主任务失败 → `post_*` 跳过
- 相位不匹配自动跳过（uninstall 不跑 install hook）
- hook 用 `run_once: true` + `delegate_to: localhost` 做"全局只执行一次"的初始化/通知

### release marker

deploy 成功后每台主机写入：

```
<marker_dir>/<chart名>/release.json
```

内容：chart 名 / 版本 / values 摘要（sha256 前 12 位）/ 部署时间 / wdp 版本。uninstall 成功后自动清除。这是 status 相位与未来漂移检测的数据地基；`no_marker: true` 禁用。

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

## 生成器：wdp new

```sh
wdp new myapp                 # 最小骨架：values/deploy/uninstall/status/helpers/templates/envs
wdp new myapp --full          # 全能力参考（下表全部能力均有可运行示例）
wdp new myapp --dir /opt      # 指定生成目录（默认 .）
wdp new --module user         # 查看模块参数文档与示例片段（等价 wdp modules user）
```

生成物保证 `wdp lint` 通过且 `--check` 本机演练可跑通（质量门随 CI 回归）——直接填写即可使用。`--full` 覆盖：三层 values、required、helpers、子 chart 版本约束、strategy（金丝雀+门+回滚）、hook、delegate_to、run_once、loop_var、block/rescue、group_by 动态分组、8 个新模块、output 控制。

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
