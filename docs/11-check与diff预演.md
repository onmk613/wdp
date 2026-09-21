# 11 Check 与 Diff 预演

## --check：零风险预演

所有模块只做**只读探测**并返回真实变更预估，不做任何变更：

```sh
wdp run site.yaml --check -i inv.yaml
wdp run ./myapp --check -i inv.yaml -f envs/prod.yaml
wdp run ./myapp --check --phase uninstall -i inv.yaml   # 卸载预演
wdp adhoc -m service -a 'name=nginx state=restarted' --check webservers
```

各模块的 check 行为：

| 模块 | 预估依据 |
|---|---|
| shell / command | creates/removes 守卫判断；无守卫报告"将执行" |
| script | 报告"将执行"（不上传不执行） |
| copy / template | 远端 sha256 对比 + 权限/属主漂移 |
| file | 状态/权限/属主/链接目标漂移 |
| package | 逐包探测"将安装/将卸载" |
| service | is-active / is-enabled 对比 |
| user / group | 逐属性漂移探测 |
| systemd_unit | 单元文件对比 + 服务状态预估 |
| unarchive | creates 守卫 |
| get_url | 远端校验和对比 |
| lineinfile | 行级变更计算 |
| wait_for | 单次探测报告就绪状态（不等待） |
| stat / setup / group_by / fetch | 只读，行为不变 |
| 脚本模块 | 注入 `WDP_CHECK=1`，由脚本自行返回预演 |

check 模式下 RECAP 明确标注：`check 模式：changed 为变更预估（--diff 可看内容级差异）`。

## --diff：内容级差异

`--diff` 自动启用 check，并输出变更前后对照（控制台红绿着色，JSON 模式为结构化 `diff` 字段）：

```sh
wdp run ./myapp --check --diff -i inv.yaml -f envs/prod.yaml
```

| 模块 | diff 形态 |
|---|---|
| copy / template / get_url | unified diff（远端内容 vs 目标内容；远端缺失时全部为新增行） |
| lineinfile | 行级 unified diff |
| file | 属性 before→after（mode/owner/group/link target） |
| package | 逐包 `+`/`-` 清单 |
| service / systemd_unit | active/inactive、enabled/disabled 前后对照 |
| user / group | uid/shell/groups 等属性前后对照 |
| unarchive | 目标目录状态变化说明 |

控制台示例：

```
TASK [下发配置 (template)] ********************
changed: [web1]: [check] /etc/app/app.conf 将写入（218 字节）
    --- 远端 /etc/app/app.conf
    +++ 目标 /etc/app/app.conf
    @@ -2,3 +2,3 @@
     env=prod
    -port=8080
    +port=9090
```

说明：

- 内容 diff 限制远端文件 ≤ 1MB（超出仅显示变更摘要）
- diff 展示遵循任务级 `output` 控制（敏感任务 `no_log: true` 同样隐藏 diff）
- 聚合模式下带 diff 的预估结果也会显示（不只是异常）

## release diff：两次部署的参数对比

不碰主机，对比本地部署记录的 values 快照：

```sh
wdp release list                      # 找到两次记录的 id
wdp release diff myapp-1712340000 myapp-1712400000
```

输出逐路径变更（`-` 旧值 / `+` 新值，含新增/删除标记），回答"这次升级会改哪些参数"。

## 预演 → 决策 → 执行的推荐循环

```sh
wdp lint ./myapp                                   # 1. 静态校验
wdp run ./myapp --check --diff -i inv.yaml -f envs/prod.yaml   # 2. 预演 + 差异评审
wdp run ./myapp -i inv.yaml -f envs/prod.yaml                   # 3. 执行
wdp release diff <上一次id> <本次id>                              # 4. 事后参数审计
```

CI 集成：`--output json --check` 输出结构化预演结果（含逐主机 diff 字段），可直接作为门禁输入。
