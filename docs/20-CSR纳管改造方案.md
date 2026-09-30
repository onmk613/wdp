# 20 纳管私钥 CSR 化改造方案（目标机本地生成）

> 状态：**已实施**（按 §2/§3 设计落地，无兼容降级路径——旧 host-cert/
> host-key 端点已删除，脚本直切 CSR 流程；§4 的兼容矩阵不再适用）。
> 这是 docs/18 P0-2 的根治手段。

## 1. 背景与威胁模型

当前纳管链路（`web/enroll.go`）：

```
目标机脚本 ──POST /enroll/{token}/claim──► server：生成 keypair 并落盘
          ◄─GET  host-cert / host-key───  （key 经 URL 换出，一次性 + 绑定 claim IP）
```

私钥由 **server 生成**，经 HTTPS 交付。已收敛的攻击面（docs/18 P0-2 修复）：

- 明文通道默认拒绝下发（`--allow-plaintext-enroll` 显式豁免）；
- 私钥交付绑定 claim 来源 IP 且只交付一次（`key_delivered_at`）。

**剩余固有弱点**（CSR 化要消灭的）：

1. 私钥明文存在于 server 磁盘（`<caDir>/hosts/<name>.key`）与传输通道中——
   反代访问日志只挡不住 body 落盘类的日志/镜像泄漏；
2. token 一旦在有效期内被第三方于**同一来源 IP** 抢用（NAT 后同出口），
   旧流程交付的是可长期冒充该主机的完整身份；
3. server 侧持有全部 agent 私钥，CA 目录失陷 = 全网身份失陷。

CSR 化后：私钥**永不离开目标机**，server 只见过公钥。上述三点同时消失；
CA 目录失陷只暴露 CA 私钥（可吊销轮换），不再附带全网叶子私钥。

## 2. 协议设计

新流程（替换 claim 之后的证书三件套下载）：

```
目标机脚本
  1-2. 下载 CA + 二进制（不变，指纹校验同现有）
  3.   POST /enroll/{token}/claim              （不变：记 hostname/来源 IP）
  3b.  $TMP/wdp agent gencsr --key $ETC_DIR/agent.key --csr $TMP/agent.csr
         ↑ 目标机本地生成 ed25519 keypair + CSR（CN 留空，SAN 不自报）
  4.   POST /enroll/{token}/csr  body=PEM(CSR)
         ◄─ 200 PEM(cert)   SAN/有效期/档案全部由 server 侧 claim 身份决定
  5-6. 落盘/systemd/done（不变；host-key 下载步骤删除）
```

**服务端签发规则**（安全核心，全部在 server 侧决定，CSR 只贡献公钥）：

- `csr.CheckSignature()` 必须通过（防"拿别人 CSR 冒名"）；
- CSR 自带的 SAN/CN **全部忽略**——SAN 按 `issueHostCert` 现行口径
  由 `ClaimAddress/ClaimHost/HostName` 重建（复用孤儿证书覆盖判定）；
- 公钥算法限制 ed25519/ECDSA P-256（拒绝 RSA 弱参数与超大键）；
- 档案/有效期沿用 `ProfileServer` + `DefaultAgentCertDays`。

**端点门禁**（与 host-key 现行口径完全一致，直接复用）：
token 未用未过期、`claim the token first`、交付必须来自 claim 同一来源 IP。
`key_delivered_at` 语义改为 `csr_accepted_at`（防同一 token 换第二把钥：
CSR 接受后 token 锁定到该公钥，重复提交同公钥幂等返回同一证书，
不同公钥 410）。

## 3. 组件改造清单

| 组件 | 改动 | 要点 |
|---|---|---|
| `internal/ca` | 新增 `SignCSR(o SignCSROptions, name string) (crtPath string, err)` | 复用 `leafTemplate` + `signAndWrite`；`CreateCertificate(pub=csr.PublicKey)`；只写 `.crt` 不写 `.key`（server 无钥可写）。单测：CSR 签名校验、SAN 忽略、算法白名单 |
| `internal/cli`（agent 命令组） | 新增 `wdp agent gencsr --key <path> --csr <path>` | 目标机本地执行：生成 ed25519 key（0600）、`x509.CertificateRequest{PublicKey}` DER→PEM。已存在 key 时**幂等复用**（重装场景不换身份）。单测：幂等、权限位 |
| `internal/web/enroll.go` | 新增 `POST /enroll/{token}/csr`；`serveHostCertFile` 的 `.key` 分支与 `handleEnrollHostKey` 路由**保留一版**作降级通道（见 §4）；`issueHostCert` 拆出 `hostSANsOf(t) []string` 供两条路径共用 | 门禁逻辑照抄 §2；证书仍落 `<caDir>/hosts/<name>.crt`（server 侧留档便于续期/inspect，续期路径 `certrenew` 需要读旧证书） |
| `internal/store/enroll.go` | `enroll_tokens` 增列 `csr_pubkey_sha TEXT NOT NULL DEFAULT ''`（迁移 v14） | 首次接受 CSR 时写入；非空且与本次不同 → 410 |
| 脚本模板 `enrollScriptTmpl` | 步骤 3b/4 替换 host-cert/host-key 下载；**能力探测降级**：`if "$TMP/wdp" agent gencsr --help >/dev/null 2>&1; then CSR 路径; else 旧三件套路径; fi` | bin 目录可能是旧版二进制（server 与 bin 目录独立升级），不做探测会让旧二进制纳管直接失败 |
| `internal/web/sshinstall.go` | 二期：推装路径改推 `gencsr`（经 SSH 执行目标机二进制生成、取回 CSR 签发）| 一期先保留 server 生成 + SFTP 推送（SSH 通道本身加密且单向可信，风险等级低于 URL 换出） |

## 4. 兼容矩阵

| server | bin 目录二进制 | 行为 |
|---|---|---|
| 新 | 新 | CSR 路径（探测命中） |
| 新 | 旧 | 探测失败 → 旧三件套路径（host-key 端点保留一版的原因） |
| 旧 | 新 | 脚本是旧模板（server 生成）→ 新二进制跑旧流程，正常 |

删除旧端点的时机：bin 目录全量更新后再版（major 版本），或在
`--enroll-legacy-key` 开关后一个版本。**不做**无探测的硬切换。

## 5. 明确不改的部分

- **续期/换证**（`certrenew`、`agentctl renew`）：本来就保留目标机密钥对、
  server 只重签证书，与 CSR 产物天然一致，零改动；
- **ctl 证书**：控制端身份，数量为 1，server 持钥可接受；
- **退役/吊销**（`retireAgent`）：按证书指纹操作，与私钥在哪生成无关。

## 6. 实施顺序（每步独立可合入）

1. `ca.SignCSR` + 单测（纯函数，无流程影响）；
2. `wdp agent gencsr` 子命令 + 单测；
3. 迁移 v14 + `POST /enroll/{token}/csr` 端点 + 门禁单测
   （复用 `enroll_hardening_test.go` 的攻击用例骨架：过期/已用/来源不符/
   异公钥抢注/CSR 签名无效）；
4. 脚本模板改造（含能力探测）+ `TestEnrollFullFlow` 增加 CSR 分支断言
   （server 磁盘不再出现 `<name>.key`）；
5. 前端纳管对话框文案（一键命令展示不变——变化全在脚本内部）；
6. 二期：SSH 推装路径 CSR 化 + 下线 legacy host-key 端点。

## 7. 验证口径

- 回归：`TestEnrollFullFlow` 双分支（新/旧二进制模拟）；
- 安全：`enroll_hardening_test.go` 全量攻击用例对 CSR 端点重放；
- 断言 server 侧 `<caDir>/hosts/` 只增 `.crt` 不再增 `.key`（新路径）；
- agent 启动加载 CSR 产物证书与旧产物证书行为一致（mTLS 握手 e2e）。

## 8. 实施记录（与 §3/§5/§6 设计的偏差）

| 项 | 实际落地 |
|---|---|
| 兼容策略 | 按决策**不做兼容**：host-cert/host-key 端点与 key_delivered_at 语义整体删除（列保留不重建表），脚本无能力探测分支 |
| SSH 推装 | 一期即 CSR 化（原设计列为二期）：上传二进制 → 远端 `gencsr` → 取回 CSR → 签发 → 回传证书，私钥不经 SSH 传输 |
| 续期 | §5"零改动"结论修正：certrenew 此前读 server 侧 .key 配对校验，CSR 纳管后新主机无此文件——`ca.Renew` 增加 **Keyless 模式**（重签只需旧证书公钥，对存量/新主机统一生效） |
| 幂等 | 既有证书"覆盖 claim 身份**且同公钥**"才复用；覆盖但异钥 = 同机丢钥重装，按 claim 身份换新公钥重签（旧流程的 certCoversClaim 归属判定保留） |
| 回归测试 | `ca/csr_test.go`（验签/算法白名单/SAN 忽略/无钥续期/幂等）、`web/enroll_test.go` 全流程（含 server 侧无 .key 断言）、`web/enroll_hardening_test.go`（来源绑定/异钥 410/坏 CSR 400）、`store/enroll_csr_test.go`（token 公钥锁定） |
