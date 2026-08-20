# examples 示例索引

全部示例均可直接运行：playbook 与 inventory 默认走本机 `conn: local`（无需任何远端），
系统级模块（package/user/group/service/systemd_unit）在非 Linux 主机由 `when` 守卫自动跳过。
本目录自带回归测试（`examples_test.go`，`go test ./examples/`）——所有示例必须保持可运行。

```sh
# 以下命令均假定在仓库根目录执行，二进制为 go build -o bin/wdp ./cmd/wdp
```

| 示例 | 看点 | 快速开始 |
|---|---|---|
| [site.yaml](site.yaml) | 入门 playbook：模板→分发→执行→回拉→handler | `wdp run examples/site.yaml -i examples/inventory.yaml --limit web-local` |
| [inventory.yaml](inventory.yaml) | 三种连接类型（ssh/agent/local）的清单写法 | `wdp adhoc -m shell -a 'uname -a' -i examples/inventory.yaml web-local` |
| [features/](features/) | playbook 特性矩阵（见下表） | 逐文件见下 |
| [inventory-demo/](inventory-demo/) | 变量合并优先级、多文件合并、模式选择 | 见下 |
| [chart-demo/](chart-demo/) | chart 基础：三层 values、子 chart、生命周期 | `wdp run examples/chart-demo -i examples/chart-demo/inventory.yaml` |
| [webstack/](webstack/) | **代表性应用**：复杂配置下的自动化部署全要素 | 见下 |
| [bench/](bench.yaml) | 千台主机压测 playbook（配合 scripts/gen-inventory.sh） | `wdp run examples/bench/bench.yaml -i <(./scripts/gen-inventory.sh 1000) --forks 200` |
| [wdp-agent.service](wdp-agent.service) | agent 常驻部署的 systemd 单元 | 见文件头注释 |

## features/：特性矩阵

`local.yaml` 是通用演练清单（4 台本机主机 + 每台独立的 `workdir` 主机变量）。

| 文件 | 覆盖内容 |
|---|---|
| [modules.yaml](features/modules.yaml) | 19 个内置模块逐一使用：幂等守卫、Linux 守卫、stat+fetch 联动、group_by 动态组 |
| [task-controls.yaml](features/task-controls.yaml) | 任务级控制属性全集：when/loop（含 map 项与自定义变量名）/register/notify/tags/environment/ignore_errors/until/timeout/changed_when/failed_when/delegate_to/run_once/output/no_log |
| [block-rescue.yaml](features/block-rescue.yaml) | block/rescue/always、嵌套容错、`block_failed_msgs`、ignore_errors 不触发 rescue |
| [serial-strategy.yaml](features/serial-strategy.yaml) | serial 三种写法 + canary/rolling 策略 + 健康门 + 自动回滚（成功路径） |
| [rollback-demo.yaml](features/rollback-demo.yaml) | 健康门失败 → 自动回滚 → 终止批次（**预期失败**的对照示例） |
| [dynamic-groups.yaml](features/dynamic-groups.yaml) | setup facts → group_by 建组 → 通配 hosts 引用 → `.groups`/`.hosts` 内置变量取拓扑 |

```sh
INV=examples/features/local.yaml

# 零风险预演（推荐先跑）
wdp run examples/features/modules.yaml -i $INV --check --diff

# 真实执行（写 /tmp，复跑验证幂等收敛）
wdp run examples/features/modules.yaml -i $INV

# 任务筛选 / 断点续跑
wdp run examples/features/task-controls.yaml -i $INV --tags install
wdp run examples/features/task-controls.yaml -i $INV --start-at-task "until 轮询（.result 引用当轮结果）"

# 亲眼看自动回滚：金丝雀部署 → 门失败 → 文件被删 → 后续批次终止
wdp run examples/features/rollback-demo.yaml -i $INV   # 退出码非零是演示点
```

## inventory-demo/：变量体系

```sh
D=examples/inventory-demo

# 变量优先级可视化：all.vars < group_vars < host_vars（web1=8081 覆盖组变量 8080）
wdp run $D/show-vars.yaml -i $D/inventory.yaml

# 多文件合并：env→prod、追加 web3、组变量深合并
wdp run $D/show-vars.yaml -i $D/inventory.yaml -i $D/inventory-prod.yaml

# 主机模式选择：通配 / 排除 / 交集
wdp run $D/show-vars.yaml -i $D/inventory.yaml --list-hosts --limit 'web*'
wdp run $D/show-vars.yaml -i $D/inventory.yaml --list-hosts --limit 'webservers,!web1'
wdp run $D/show-vars.yaml -i $D/inventory.yaml --list-hosts --limit 'production:&webservers'
```

## webstack/：代表性应用（复杂配置下的自动化部署）

**orders-api**（Go Web 服务）+ **Nginx 反向代理**的生产风格应用包，一台命令完成
「系统用户 → 版本化发布 → 配置渲染 → systemd 服务化 → 健康门金丝雀 → 反代自动发现」：

```sh
W=examples/webstack; INV=$W/inventory.yaml

wdp template $W -f $W/envs/prod.yaml        # 预览：合并 values + 模板渲染 + 任务树
wdp run $W -i $INV --check --diff           # 零风险预演（内容级差异）
wdp run $W -i $INV                          # 本机真实部署（local 通道）
wdp run $W -i $INV --phase status           # 只读状态探测
wdp run $W -i $INV --phase uninstall        # 逆操作卸载（含 marker 清理）
```

| 复杂配置要素 | 在 webstack 中的位置 |
|---|---|
| 三层 values 深合并 | `values.yaml` → `envs/prod.yaml`（cache.driver 换 redis 而 ttl_secs 保留）→ `--set app.log.level=debug` |
| required 配置门禁 | `chart.yaml` 声明 6 个必填点路径（`--set app.version=null` 可看报错） |
| 子 chart 组件化 | `charts/common@1.x`（系统层）+ `charts/nginx@2.x`（反代），版本约束引用 |
| global 跨 chart 共享 | `global.app_port`：nginx 子 chart 的 upstream 端口来自父作用域 |
| 拓扑自动发现 | nginx.conf 的 upstream 由 `.groups`/`.hosts` 内置变量渲染，appservers 增减主机免改配置 |
| 金丝雀 + 健康门 + 回滚 | deploy.yaml play 1 的 `strategy:`（canary/batch/gate/auto_rollback） |
| 生命周期 hook | pre_install 数据迁移（script 模块）→ post_install 发布通知 |
| 脚本模块扩展 | `modules/orders-health`（JSON 契约自检，无需改 wdp 源码） |
| 版本化发布 | `releases/<version>/` + `current` 软链原子切换（unarchive creates 守卫） |
| systemd 服务化 | `templates/orders-api.service.tpl`（内容变更自动 daemon-reload） |
| 环境差异化路径 | nginx `conf_dir`：演练环境 /tmp，生产 /etc/nginx/conf.d（envs/prod.yaml 覆盖） |

生产部署形态（真实远端 + 生产环境值）：

```sh
wdp run $W -i inventory-prod.yaml -f $W/envs/prod.yaml \
  --set app.secrets.api_token=$(cat /run/secrets/orders_token)   # 敏感值不进仓库
```

## 运行环境说明

- 演示用 local 通道的示例在任何 POSIX 主机可跑（macOS/Linux CI 均验证）；
  系统级模块的真实执行需要 Linux 目标机（become 需 root 或 sudo）。
- `webstack` 生产环境值（`envs/prod.yaml`）将 workdir 指向 `/opt/orders`、
  conf_dir 指向 `/etc/nginx/conf.d`——在无 root 的本机直接跑会在建目录时失败，
  属预期行为；本机演练用默认 values 即可。
- 压测：`scripts/gen-inventory.sh 1000` 生成千台清单后配合 `examples/bench`。
