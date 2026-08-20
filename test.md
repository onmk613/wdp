wdp 项目评价报告
我用 4 个并行审计 + 亲手复现的方式检查了整个项目（~20k 行 Go，105 个文件）。所有 CRITICAL/HIGH 结论我都亲自跑出来了，不是静态推测。

总体结论
这是一个水平明显高于平均的项目。 架构分层干净、无循环依赖，安全基本功扎实（很多成熟项目做不到），测试通过 -race。它的问题不在"写得糙"，而在语义正确性偏离 Ansible —— 而这恰恰是"Ansible 替代品"最不能错的地方。用户会按 Ansible 直觉写 playbook，然后被静默坑到。

基线：go build / go vet / gofmt -l / go test ./...（17 包）/ go test -race 全绿。

必须先修的（我都复现了）
1. chart 自引用 → 进程崩溃（CRITICAL） runChartTask → runTaskOnHost 递归无深度限制、无 visited 集合。实测 fatal error: stack overflow，exit 2。Go 的栈溢出 recover() 拦不住，一个畸形 chart 直接杀死编排器。修：传 depth + visited，chart lint 加环检测。

2. handler 在没通知它的主机上执行（HIGH，影响最大） collectNotified（executor.go:1265）把所有主机的通知并成一个集合，然后每个 handler 扇出到全部存活主机。实测：只有 h1 变更，handler 在 h1 和 h2 都跑了。

生产后果：一台机器改了配置，全集群重启服务。

3. 失败主机仍参与后续 play（HIGH） 实测：h1 在 play 1 "install" 失败，play 2 "start service" 照样在 h1 执行。对部署工具来说这是危险语义。

4. no_log: yes 静默失效，泄露密钥（HIGH） inventory.go:452 / playbook.go:248 等 9 处用 x, _ = v.(bool) 丢弃 ok 标志。YAML 1.1 写法 yes/on/"true" 解析成字符串→断言失败→静默取 false。实测 no_log: yes 在 -v 下明文泄露，no_log: true 才生效。同一模式让 host_key_check: yes 静默关闭 SSH 中间人防护（覆盖掉 419 行算好的安全默认值）。这偏偏是 Ansible 用户的习惯拼写。

5. fetch 路径穿越，可写控制端任意文件（HIGH） fetch.go:78-84 拼 dest + Host.Name + src 无 Clean/包含性校验。实测 dest: ./dest，主机名 ../../X 把文件写到 dest 上面两层；src 带 .. 也能逃逸。fetch 的 src 常来自远端数据 → 被控节点可反向写控制端（那里有 ca.key、SSH 私钥）。同项目 chart.go:169-173 已有正确写法，照抄即可。

6. 模板可读控制端环境变量（HIGH） engine.go:28 用了完整 sprig.TxtFuncMap()，含 env/expandenv。实测 {{ env "..." }} 打印出控制端变量值 —— 配合模板渲染的 shell: 即可外泄 WDP_CA_PASSPHRASE 和所有 *_env 密钥。Helm 正是为此移除了这两个函数。

7. when 跳过不写 register / 空 loop 不给 results（HIGH） 实测两者都让下游模板硬报错 map has no entry for key。when: not r.skipped 是 Ansible 最常见惯用法，空列表循环也很常见。

其他确认项
问题	实测
模块参数零校验	moed:/ownerr: 拼错被静默忽略并报成功。19 个模块都有 Params() 元数据但执行器从不校验 —— 一处约 15 行可消除整类问题
retries 语义不一致	无 until = 3 次（retries+1）；有 until = 4 次（仅 retries）。Ansible 应为 5 次
always 不执行	主机 unreachable 时 always 被跳过 —— 恰好反转了"保证清理"的契约
push 模式明文 HTTP	--listen 0.0.0.0 + http://，token 与 become 密码网络明文（README 承认是设计取舍，但相对刚用过的 SSH 是降级）
forks 语义	信号量在 go 之后获取，每主机立即建 goroutine（并发执行有界，goroutine 无界）
Ctrl-C	play/batch/task 三层循环均不查 ctx.Err()，中断后继续跑完并报一堆失败
i18n 竞态	Resolve() 写全局 lang 不加锁，而 T() 读时加锁
MustRender	panic 版本是死代码（零调用者）
值得肯定的地方（不是客套）
分层无环：model/render 是叶子，依赖单向流动；shellquote 集中成 13 行且每个插值点都用了，SSH 载荷 base64 包装、env key 正则白名单 —— 我没找到注入口
安全基本功：crypto/subtle 常量时间比较、TLS 1.2 下限、RequireAndVerifyClientCert、指纹 pinning、非回环无认证拒绝启动、sudo -S 走 stdin 让密码不进 argv（且有测试锁住这个不变量）
CA 实现正确：每次加密独立随机 salt+nonce（避开 GCM 经典陷阱）、PathLen=0、90 天短证书
连接池：握手在锁外 + 落表双检 + 输家自关，注释解释了 why —— 比多数手写池好
--check 真的不改东西（copy/shell 实测）、主机选择 DSL（! :& web* db?）与文档完全一致、tar-slip 防护连符号链接变体都堵了
-race 全绿、README/docs 详实（2800 行）、19 个模块数量与文档相符
后期维护建议（按性价比）
补 CI + git init —— 目前没有 git 仓库、没有 CI、没有 LICENSE。这是最大的工程风险：上述回归本可被 CI 拦住。加 go test -race、go vet、golangci-lint、govulncheck
加语义一致性测试套件：把"与 Ansible 行为对齐"变成断言（handler 作用域、skip 后 register、空 loop、retries 次数、跨 play 隔离）
禁掉静默 _ = v.(bool)：统一 toBool 严格解析（接受 yes/no/on/off），类型不符报错而非 fail-open
拆 executor.go：1460 行、runTaskOnHost 356 行含 177 行嵌套闭包和两个重试循环 —— 复杂度正是上面语义 bug 藏身之处。同时把编排里手写的 POSIX shell（rm -rf/mkdir/cp -a）收进传输层端口
补测试薄弱区：agentconn/localconn/pushagent/model/release 测试比 0.00，cli 0.07，inventory 0.18（680 行合并+选择逻辑）。executor 0.91、shellquote 4.61 是好的
错误链：executor 里 %w 用了 0 次（playbook.go 用了 14 次），fail() 把错误拍平成字符串，下游无法 errors.Is/As 区分超时/认证/模板错误
优先级：1(崩溃) → 2/3(误伤生产) → 4/5/6(安全) → 7(静默破坏 playbook)。第 4 项 no_log/host_key_check 和第 11 项参数校验修起来很小，但消除的是整类静默失效。

仓库我已还原原状：删除了所有临时目录和 bin/wdp-audit，并修回了子代理 go mod tidy 意外升级的 go.mod/go.sum（semver 3.3.0、mergo 1.0.1、cast 1.7.0），复验全绿。

两点说明：本机 Go 1.26.5 的 go test -cover 坏了（no such tool "covdata"，环境问题非代码问题），所以覆盖率我用测试/代码行数比替代；Hindsight 记忆库本次返回 401 未认证，结论未能落库。

Repo pristine again. Here are the amendments from the module audit's full report.

## 报告补充（模块层审计）

我逐条复现了新报告的关键结论。**一条不成立，需更正；三条新增确认。**

### ❌ 更正一条（我不采纳）

**F-17「`mode: 0755` 被当十进制读成 `01363`」不成立。** 实测 `yaml.v3` 把 `0755` 解析为 `int 493`（正确八进制），`fs.FileMode(493).Perm()` = `0755`。这与我之前用真实二进制跑出的结果一致（文件确实得到 `rwxr-xr-x`）。该结论把 YAML 1.1 规则误套到 `yaml.v3` 上了。

不过同一函数里有个**真问题**：`0999` 被 `yaml.v3` 解析成 `float64(999)`，`argMode` 的 `switch` 没有 `float64` 分支 → 静默返回 `ok=false`，**mode 被整个丢弃**。属于同一类"静默失效"。

### ✅ 新增确认三条

**1. `backup: yes` 静默不备份（HIGH，与 `no_log: yes` 同根）**
排除了一个混淆项后（`local` 通道下相对路径 `dest` 不解析到 CWD，导致备份分支根本不进），用绝对路径干净复现：

| 写法 | 生成备份数 |
|---|---|
| `backup: true` | **1** |
| `backup: yes` | **0** |

用户以为有备份、实际没有，然后文件被覆盖。这让 `argBool` 的静默丢弃从"配置没生效"升级为**数据丢失风险**，与前述 `no_log`/`host_key_check` 是同一个 `_ = v.(bool)` 病灶——**修一处、收三利**。

**2. `file` 模块自带示例是坏的（MEDIUM）**
`file.go:300-304` 的 `Example()` 用了 `dest:`，但 `Run` 只读 `path`（`file.go:30`），`Params()` 也只声明 `path`。原样运行：

```
local  fatal   file 需要 path 参数
```

因为 `Example()` 会被 `wdp new --module` 写进用户生成的 playbook（`skel.go:113-120`），**这是在向用户分发一段跑不通的代码**。根因是 `file`/`lineinfile` 用 `path`，而 `copy`/`template`/`get_url`/`unarchive` 用 `dest` —— 建议互设别名。

**3. `package` 的 `state: latest` 永不收敛，且 check 与实跑自相矛盾（CRITICAL）**
代码确认：check 分支（`package.go:54-65`）只判 `installed`，**没有 `latest` 分支**，已安装即落入 `default` → 报"已是目标状态、无变更"；实跑分支（`package.go:90-95`）则**无条件** `upgrade` 并硬置 `changed = true`。

于是 `--check` 说"干净"，真跑却每次都 `changed` —— 直接破坏 `docs/11-check与diff预演.md` 承诺的预演保真，并让 `notify`/handler **每次部署都误触发**。这与我确认的 handler 扇出 bug 叠加，后果会放大。

### 值得记录的旁证

`local` 通道下**相对路径 `dest` 不以 playbook 目录为基准**（`./bk.txt` 走到别处），这是我排查时撞见的独立行为差异，用绝对路径即绕开——但对"本机演练/CI"这个被 README 推荐的用法来说值得留意。

### 与我原报告的关系

原报告的优先级不变，只是证据更强：
- **第 4 项（`_ = v.(bool)` 静默 fail-open）** 现有三个已复现实例：`no_log: yes` 泄密、`host_key_check: yes` 关闭 MITM 防护、`backup: yes` 丢备份。这仍是**性价比最高的修复**。
- **CRITICAL 清单**从 1 项（chart 递归崩溃）增至 2 项，新增 `package latest` 的 check/实跑矛盾。
- 该报告与我独立结论**在参数零校验（`moed`/`ownerr` 静默忽略）上完全吻合**，且它给出了可行修法：复用现成 `Params()` 元数据，在执行器加约 15 行做未知键拒绝。

仓库已再次还原：删除 `.v2`/`.v3` 与临时二进制，`go.mod`/`go.sum` 仍为原始 884 B / 62 行，`gofmt` 干净，`internal/module` 测试通过。