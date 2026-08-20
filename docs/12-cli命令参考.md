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
| `--lang` | auto | 输出语言：auto / zh / en（或环境变量 `WDP_LANG`；作用于帮助、模块文档与提示文案） |

未显式指定的 flag 回退 wdp.cfg（见[配置文件](02-配置文件.md)）。

## 命令分组（`wdp --help` 分类展示）

| 分组 | 命令 |
|---|---|
| 部署命令 | `run`、`adhoc` |
| 应用包命令 | `new`、`template`、`lint`、`package` |
| 安全与信任命令 | `ca`、`key` |
| 代理命令 | `agent` |
| 运维与记录命令 | `release`、`modules` |
| 其它命令 | `version`（另有框架自带的 `help`、`completion`） |

## wdp run

```
wdp run <playbook.yaml | chart目录 | chart.tgz>
        [-i inventory（可多次）] [-f values 文件（可多次）] [--set k=v（可多次）]
        [--limit 模式] [-t/--tags 逗号分隔] [--skip-tags 逗号分隔]
        [--check] [--diff] [--list-hosts] [--start-at-task 任务名]
        [--phase deploy|uninstall|status] [-y/--yes]
```

| flag | 说明 |
|---|---|
| `-f / --values-file` | chart values 覆盖文件，按序深合并（同 Helm） |
| `--set` | 点路径覆盖（`--set a.b[0]=v`；`k=null` 删键） |
| `--limit` | 在 play hosts 基础上进一步收窄主机（选择模式语法） |
| `-t / --tags` | 仅执行带这些 tag 的任务 |
| `--skip-tags` | 跳过带这些 tag 的任务 |
| `--check` | 预演模式（全相位可用） |
| `--diff` | 内容级差异（自动启用 check） |
| `--list-hosts` | 仅列出将执行的主机 |
| `--start-at-task` | 从指定任务开始（调试） |
| `--phase` | chart 生命周期相位（deploy 缺省 / uninstall / status） |
| `-y / --yes` | 跳过不可逆操作确认（CI 建议） |

示例：

```sh
wdp run site.yaml -i base.yaml -i prod.yaml --limit 'web*,!web1'
wdp run ./myapp -f envs/prod.yaml --set app.port=9090 --check --diff
wdp run ./myapp-0.1.0.tgz -i inv.yaml --phase uninstall -y
```

退出码：0 成功；1 存在失败主机或错误。

## wdp adhoc

```
wdp adhoc -m <模块名> -a '<参数>' [--become] [--format '模板'] [--check] [--diff] <主机模式>
```

| flag | 说明 |
|---|---|
| `-m / --module` | 模块名（缺省 shell；`wdp modules` 查看列表） |
| `-a / --args` | free-form 命令或 `k=v` 参数列表 |
| `-b / --become` | 提权执行 |
| `--format` | 逐主机模板化输出（`.host .stdout .rc …`），进 shell 管道 |
| `--check` / `--diff` | 预演 / 差异 |

```sh
wdp adhoc -m shell -a 'uptime' all
wdp adhoc -m package -a 'name=curl state=present' --become webservers
wdp adhoc -m stat -a 'path=/etc/hosts' --format '{{.host}} {{.stdout}}' web*
```

## wdp new

```
wdp new <应用名> [--full] [--dir 目录] [--module 模块名] [--list-modules]
```

| flag | 说明 |
|---|---|
| `--full` | 生成全能力参考骨架（策略/hook/委托/动态分组/子 chart/全部模块有示例） |
| `--dir` | 生成目录（缺省当前目录；目标已有 chart.yaml 拒绝覆盖） |
| `--module` | 输出模块参数文档与示例片段（不生成骨架） |
| `--list-modules` | 列出全部内置模块名 |

## wdp template

```
wdp template <chart目录|tgz> [-f values 文件] [--set k=v] [--hostname 名字]
```

预览：合并后的最终 values + 全部 templates/ 渲染结果 + 任务树（chart 引用展开一层）。`--hostname` 指定预览用 `inventory_hostname` 占位（缺省 preview-host）。

## wdp lint

```
wdp lint <chart目录|tgz> [-f values 文件] [--set k=v]
```

校验：结构完整性、helpers 可解析、任务树（block 递归、chart 引用与版本约束、模块名、chart 脚本模块）、全部模板可用样例域渲染、envs 文件可解析。发现 ERROR 时退出码非零。

## wdp package

```
wdp package <chart目录> [-o 输出目录]
```

先加载校验再打包为 `<name>-<version>.tgz`（包内顶层 `<name>/` 前缀，可直接 `wdp run`）。

## wdp modules

```
wdp modules [模块名]
```

无参数列出全部模块名与说明；带模块名输出该模块的参数文档与示例任务（与实现同源）。

## wdp release

```
wdp release list [chart名前缀]
wdp release show <id> [--values]
wdp release diff <id1> <id2>
```

| 子命令 | 说明 |
|---|---|
| `list` | 部署记录列表（新在前，可按 chart 名前缀过滤） |
| `show` | 记录详情；`--values` 仅输出 values 快照 YAML（可直接 `-f` 重放） |
| `diff` | 两次部署 values 逐路径对比 |

记录存于 `~/.wdp/releases/<id>.json`。

## wdp ca

```
wdp ca init   --dir <目录> [--passphrase]
wdp ca issue  --dir <目录> --name <名称|IP> [--san <附加SAN>（可多次）] [--client] [--days N]
wdp ca renew  --dir <目录> --name <名称> [--new-key] [--days N]
```

| 子命令 | 说明 |
|---|---|
| `init` | 创建 CA（10 年，PathLen=0；私钥 PBKDF2+AES-GCM 加密落盘） |
| `issue` | 签发证书（服务端 SAN=名称/IP，默认 90 天；`--client` 签控制端客户端证书并输出 SHA256 指纹） |
| `--san` | 追加额外 SAN（可多次，IP 或域名，自动去重）——多地址/NAT/端口转发主机一张证书覆盖全部可达地址，`renew` 续期时全量继承 |
| `renew` | 原地续期（保留 SAN/EKU/私钥；`--new-key` 换钥即吊销旧证书） |

口令环境变量：`WDP_CA_PASSPHRASE`（`--passphrase` 优先）。

## wdp key

```
wdp key scan <主机模式> [--known-hosts 路径]
```

采集 SSH 主机公钥写入 known_hosts（`host_key_check` 默认开启，新主机首次连接前执行一次）。支持全部主机选择模式语法。

## wdp agent

目标机启动常驻 agent：

```
wdp agent [--listen 127.0.0.1:7602] [--token <密钥> | --token-file <文件>]
          [--ca <CA证书> --cert <服务端证书> --key <私钥>]
          [--pin-client-fp sha256:<指纹>（可多次）]
          [--allow-no-auth] [--cleanup-on-shutdown]
```

| flag | 说明 |
|---|---|
| `--listen` | 监听地址（默认仅绑定回环 `127.0.0.1`；对外监听需显式 `0.0.0.0:端口`；端口未指定时跟随 wdp.cfg `[agent].port`） |
| `--token` / `--token-file` | token 认证（token 文件读取后自动删除；`--token` 明文会进进程 argv，同机 `ps` 可见，建议用 `--token-file`） |
| `--ca/--cert/--key` | mTLS 三件套 |
| `--pin-client-fp` | 客户端证书指纹准许名单（精确吊销：移除指纹重启即拒收） |
| `--allow-no-auth` | 显式允许无认证对外监听（仅限可信内网） |
| `--cleanup-on-shutdown` | 收到 /shutdown 时删除自身二进制（push 临时 agent 用） |

安全默认：`/exec`、`/file` 提供的是目标机命令执行与文件读写原语，
**对外（非回环）监听且未配置 token/mTLS 时拒绝启动**（避免无意暴露 RCE）；
回环监听仅本机可访问，允许无认证（本地调试）。远程纳管的标准姿势：

```sh
ssh target 'wdp agent --listen 0.0.0.0:7602 --token-file /root/.wdp-token &'
```

生产建议 systemd 托管（示例：`examples/wdp-agent.service`）。

## wdp version

输出版本号。

## 退出码约定

| 场景 | 退出码 |
|---|---|
| 全部成功 | 0 |
| 存在失败/不可达主机（run/adhoc） | 1 |
| 参数/加载错误（lint 失败、未知模块、缺 required 配置等） | 1（cobra 报错输出） |
