# 05 Playbook 任务编排

playbook 是 play 列表；每个 play 选一批主机、按顺序执行任务。chart 的 `deploy.yaml`/`uninstall.yaml`/`status.yaml` 用的同一套语法。

## Play 级属性

```yaml
- name: web 部署                # 展示名（可省略，缺省用 hosts 值）
  hosts: webservers            # 必需：主机选择模式（见 inventory 文档）
  vars:                        # play 变量
    nginx_port: 8080
  environment:                 # play 级环境变量（任务的环境变量覆盖同名项）
    LANG: C
  become: true                 # 提权（任务级可覆盖）
  become_user: root
  serial: "25%"                # 分批："5" 绝对数 / "10%" 百分比 / "5,10,20" 逐批尺寸
  strategy: …                  # 部署策略（见 06 文档；不配置则 serial 语义）
  tasks: [ … ]                 # 主任务列表
  handlers: [ … ]              # 处理器（notify 触发，批次末尾 flush）
```

`serial` 三种写法：

| 写法 | 语义 |
|---|---|
| `5` | 每批 5 台 |
| `"25%"` | 每批 25%（向上取整，min 1；引号防止 YAML 当字符串外的歧义） |
| `"1,5,20"` | 逐批尺寸：1 台 → 5 台 → 20 台，最后一个尺寸对剩余主机重复 |

非法值（如 `serial: "x"`）在解析时报错，不会静默忽略。

## 任务级控制属性

```yaml
- name: 任务名
  shell: 'echo hi'             # 模块键：任务必有且仅有一个（或 chart/block）
  when: '…'                    # 条件（见下）
  loop: [a, b]                 # 循环（with_items 为别名）
  loop_control:
    loop_var: item             # 自定义循环变量名（嵌套 loop 必需）
  register: result             # 结果注册为变量
  notify: [重载 nginx]         # 变更时触发 handler（字符串或列表）
  tags: [install]              # 标签筛选
  environment: {FOO: bar}      # 任务级环境变量
  ignore_errors: true          # 失败不中断该主机后续任务
  retries: 3                   # 重试次数（配合 delay；until 时总尝试 = retries+1）
  delay: 5                     # 重试/轮询间隔秒
  timeout: 120                 # 任务超时秒（0/-1 不限）
  become: true                 # 任务级提权覆盖
  become_user: app
  changed_when: '…'            # 覆盖 changed 判定（模板表达式）
  failed_when: '…'             # 覆盖 failed 判定
  until: '…'                   # 轮询直到条件满足（.result 引用本轮结果）
  output: tail=30              # 展示控制：full/none/oneline/head=N/tail=N
  no_log: true                 # 等价 output=none（敏感任务）
  delegate_to: lb01            # 委托到另一台主机执行（结果仍归属本机）
  run_once: true               # 整批只在一台主机执行，结果复制到全部主机
  hook: pre_install            # 生命周期钩子（chart 场景）
  args: {…}                    # 显式参数 map：与 map 形式模块参数合并；
                               # 注意不可与简写（free-form）参数同用，需要控制属性时改用 map 形式
  vars: {…}                    # 仅 chart 引用任务（chart: xxx）可用
```

### 条件 when

Go 模板表达式，渲染结果经 `Truthy` 判定（空/false/0/no/off/[]/{} 为假）。列表形式为 AND：

```yaml
when: '{{ if ge .app.replicas 2 }}yes{{ end }}'
when:
  - '{{ ne .os.family "unknown" }}'
  - '{{ .st.exists }}'
```

> 未定义变量直接报错（missingkey=error，尽早暴露拼写问题）。
> 可选键用 sprig 的 `dig`：`{{ dig "mirror" "默认值" . }}`。

### 循环 loop

```yaml
- name: 逐实例部署
  shell: 'echo instance {{ .item }}'
  loop: ["1", "2"]
  register: inst               # loop 注册结果含 results 逐项数组

- name: 嵌套循环
  shell: 'echo {{ .outer }}-{{ .inner }}'
  loop: [a, b]
  loop_control: {loop_var: outer}
  # 内层任务（block 展开或下一任务）用 loop_var: inner
```

- `loop` 支持模板：单个模板元素渲染结果为 JSON/YAML 列表语法时展开为多项（如 `loop: '{{ to_json .vals }}'`）；非列表语法的渲染结果保持单元素
- 每项结果可独立 failed/changed；聚合结果 stdout 为逐项拼接、rc/msg 取最后一项
- `changed_when`/`failed_when` 作用于**聚合后**的结果

### register：结果注册

注册的变量结构：

```yaml
result:
  changed: true
  failed: false
  rc: 0
  stdout: "…"
  stderr: ""
  msg: "…"
  skipped: false
  results: [...]   # 仅 loop 任务：逐项同结构
```

```yaml
- shell: 'cat /etc/app/version'
  register: ver
- shell: 'echo current {{ .ver.stdout }}'
```

register 变量**跨批次、跨 play 延续**（serial 分批时第一批注册的变量第二批可用）。

### block / rescue / always 容错组

```yaml
- name: 容错部署
  block:
    - shell: './deploy.sh'
  rescue:
    - shell: './rollback.sh'     # block 内失败才执行
  always:
    - shell: './notify.sh'       # 恒执行
```

- 支持嵌套；block 内失败即转 rescue（ignore_errors 例外）
- rescue 任务可用 `{{ .block_failed }}` / `{{ .block_failed_msgs }}` 获取失败信息
- rescue 成功则整组视为已恢复

### delegate_to 委托执行

任务改在指定主机（或 `localhost`）上执行，**变量域与结果归属保持原主机**：

```yaml
- name: 从 lb 摘除本机
  shell: 'curl -s -X POST "lb/api/remove?host={{ .inventory_hostname }}"'
  delegate_to: lb01

- name: 控制端本地操作
  shell: 'tar czf {{ .inventory_hostname }}.tgz artifacts/'
  delegate_to: localhost
```

### run_once 单次执行

整批只在一台存活主机执行；register 结果与 notify 触发同步复制到其余主机：

```yaml
- name: 集群初始化（只跑一次）
  shell: './cluster-init.sh'
  run_once: true
```

### until 轮询等待

```yaml
- name: 等待服务就绪
  shell: 'curl -sf http://localhost:8080/health'
  until: '{{ if eq .result.rc 0 }}ok{{ end }}'
  retries: 10       # 重试次数：总尝试 = retries+1（缺省 3 次）
  delay: 3          # 间隔秒
  timeout: 120      # 任务级超时兜底
```

条件满足采用当轮结果；重试耗尽按失败计（即使最后一轮模块本身成功）。
`.result` 可引用 `rc/stdout/stderr/changed/failed/msg`。

### 生命周期 hook（chart 场景）

```yaml
- name: 仅首次安装需要的数据初始化
  shell: './init-data.sh'
  hook: pre_install        # pre_install | post_install | pre_uninstall | post_uninstall
```

- `pre_*` 在主任务序列前执行；`post_*` 在 play 全部成功（含 handlers flush）后执行
- 相位自动过滤：`--phase uninstall` 时 install hook 跳过
- 主任务失败时 `post_*` 不执行
- hook 内 register / 回滚日志与主流程贯通

## Handlers

```yaml
  tasks:
    - template: {src: nginx.conf.tpl, dest: /etc/nginx/nginx.conf}
      notify: 重载 nginx          # 仅当任务 changed 且未失败时触发

  handlers:
    - name: 重载 nginx
      service: {name: nginx, state: reloaded}
```

- 同名 handler 每 play 只执行一次，按声明顺序 flush（批次末尾 / 策略门之前）
- 死亡主机（失败/不可达）不参与 handler 执行

## 内置变量

执行引擎向每台主机强制注入（不可被任何静态层覆盖，子 chart 作用域同样可用）：

| 变量 | 内容 | 示例 |
|---|---|---|
| `inventory_hostname` | 自己的主机名 | `{{ .inventory_hostname }}` |
| `group_names` | 所属组名列表（排序） | `{{ .group_names }}` |
| `play_hosts` | 本次 play 全部选中主机名 | `{{ len .play_hosts }}` |
| `play_batch` | 当前批次主机名 | `{{ .play_batch }}` |
| `groups` | 组名 → 成员列表（含动态组） | `{{ index .groups "webservers" }}` |
| `hosts` | 主机名 → {name,address,port,conn} | `{{ (index .hosts "web2").address }}` |

facts 类变量（setup/stat 采集）直接进入变量域顶层（如 `.os.family`、`.cpus`、`.stat.exists`），且跨批次、跨 play、跨子 chart 作用域延续。

## 变量优先级（低 → 高）

```
all.vars < 组 vars（父→子） < 主机 vars < chart values < play vars < task vars（chart 引用 vars） < register/item
内置变量（inventory_hostname/groups/…）强制注入，不可覆盖
```

## 任务筛选

```sh
wdp run site.yaml --tags install          # 仅执行带这些 tag 的任务
wdp run site.yaml --skip-tags debug       # 跳过带这些 tag 的任务
wdp run site.yaml --start-at-task "下发配置"   # 从指定任务开始（调试断点续跑）
wdp run site.yaml --limit 'web1,db*'      # 收窄主机范围
wdp run site.yaml --list-hosts            # 只列出将执行的主机
```
