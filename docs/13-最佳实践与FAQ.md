# 13 最佳实践与 FAQ

## 推荐工作流

1. **应用一律打包为 chart**：裸 playbook 适合临时操作；可复用的部署进 chart（`wdp new` 起步，git 管理）
2. **环境差异全部走 values**：`envs/prod.yaml` / `envs/staging.yaml`，任务模板只引用 values，不写死环境
3. **上生产前四步**：`wdp lint` → `wdp template` → `--check --diff` 评审 → 执行
4. **敏感值不落盘**：inventory/命令行只留 `*_env` 引用；输出敏感的任务加 `no_log: true`
5. **升级前看参数漂移**：`wdp release diff <旧id> <新id>` 确认本次要改的参数

## 幂等模式速查

| 场景 | 写法 |
|---|---|
| 一次性命令 | `shell` + `creates: /path/to/marker` |
| 幂等配置下发 | `copy` / `template`（校验和自动幂等） |
| 系统配置行 | `lineinfile`（精确行/正则替换，重复执行收敛） |
| 二进制制品 | `unarchive` + `creates` 指向解压产物；在线制品用 `get_url` + `sha256` |
| 服务与自启 | `service` / `systemd_unit`（状态探测幂等） |
| 账号 | `user` / `group`（漂移探测，重复执行无副作用） |
| 等待依赖 | `wait_for`（端口/文件）或 `shell` + `until` 轮询 |

## chart 设计建议

- **`required` 收紧入参**：关键参数（端口、目录、外部地址）声明为必填，杜绝"忘了传参部署一半"
- **子 chart 单一职责**：组件（JDK/数据库/代理）各自成 chart，父 chart 组合并用 `chart: xxx@^1.0` 锁版本
- **`global` 只放真正共享的**：环境名、工作目录等；子 chart 私有配置走父 values 的同名子树
- **uninstall.yaml 与 deploy 对称**：deploy 创建的每样东西都写对应删除/停止任务
- **不可逆操作集中在 block 内**：让 rescue 有机会兜底；或提供 `hook: pre_install` 的备份任务
- **hook 表达"次数语义"**：只应首次安装执行的放 `pre_install`；每次成功部署后的通知放 `post_install`（配 `run_once + delegate_to: localhost`）

## 大规模主机

- `--forks 20+`（或 wdp.cfg），建连限流自动防握手洪峰
- 保持缺省聚合输出；排障时对单任务 `--start-at-task` + `-vv`
- 分批发布用 strategy（`canary` 首台金丝雀 + `gate` 健康门 + `auto_rollback`）
- `wdp scan-ssh 'web*'` 批量采集新集群指纹

## FAQ

**Q: 任务输出"未知模块"？**
内置清单看 `wdp modules`。chart 场景可把可执行脚本放 chart 的 `modules/<名>` 目录（脚本模块机制，见模块手册）。lint 会帮你提前发现。

**Q: 模板渲染报 `map has no entry for key "xxx"`？**
引擎是严格模式（未定义变量报错）。可选键用 `{{ dig "key" "默认值" . }}`；确认拼写；`wdp template` 可离线排查渲染。

**Q: SSH 连接被拒（host key 校验失败）？**
安全默认 `host_key_check: true`。新主机先 `wdp scan-ssh <模式>` 采集指纹；明确接受风险才用 `host_key_check: false`。

**Q: `owner`/`group` 设置报"需要 become"？**
属主变更需要 root。play 或任务加 `become: true`（local 通道除外——本机执行不提权）。

**Q: `--set app.tags[0][1]=x` 报不支持？**
多级下标暂不支持；复杂结构用 `-f` 文件。

**Q: 自动回滚没覆盖我的 shell 任务？**
自动回滚仅覆盖文件类变更（copy/template/file/unarchive）。过程性变更的恢复：rescue 兜底、写入 uninstall.yaml、或应用层自带回滚。

**Q: `wait_for` 端口探测通过但服务其实没就绪？**
`wait_for` 是控制端视角（目标机防火墙/回环绑定会误判）。目标机本机视角用 `shell: 'curl -sf localhost:port' + until`。

**Q: serial 与 strategy 用哪个？**
要"批次失败就停"（安全发布）用 strategy；要传统"批次失败继续"语义用 serial。strategy 的 `batch` 支持 `10%` 与缺省 25%。

**Q: 怎么只跑某个任务？**
`--start-at-task "任务名"` 从该任务开始；`-t/--tags` 按 tag 筛选。

**Q: 怎么重放上次部署的参数？**
`wdp release show <id> --values` 输出快照，`-f` 直接喂回。

**Q: register 的结果里没有模块 facts？**
setup/stat 的 facts 不进 register——它们直接并入变量域顶层（`.os.family`、`.stat.exists`）。register 拿到的是执行结果（rc/stdout/…）。

## 已知限制

- `local` 通道忽略 become；push 通道要求目标机与控制端二进制平台一致（`binary_path` 可指定预编译产物）
- become 密码在未启用 mTLS 的常驻 agent 通道经请求体明文传输（对外纳管建议 mTLS）；push 通道自举临时 mTLS，全程加密
- 常驻 agent 未配置 mTLS 的对外监听需显式 `--allow-no-auth`（明文 HTTP 仅限可信内网）
- `package` 的 `state: latest` 视为 changed（无法精确判定是否升级）
- 自动回滚快照在 play 正常结束后清理；过程性变更（shell/package/service/user）不在覆盖范围
- 无 CRL/OCSP：证书吊销靠指纹名单 + 短周期轮换
- 内容 diff 限远端文件 ≤ 1MB

## 排错路线

1. `wdp lint` / `wdp template`：结构、引用、渲染问题
2. `--list-hosts`：主机选择是否符合预期（选择模式语法见 inventory 文档）
3. `--check --diff`：变更内容是否符合预期
4. `-vvv` 单任务排障：`--start-at-task <名>` + 完整 stderr
5. 连接层：`wdp scan-ssh`（指纹）、`wdp adhoc -m shell -a true <主机>`（连通性）
