# 09 Values 与模板函数

## 三层 values 与合并规则

```
values.yaml（chart 默认） → -f 文件（按命令行顺序） → --set（按命令行顺序）
```

| 类型 | 合并行为 |
|---|---|
| map | **递归深合并**（嵌套 map 逐键合并） |
| 标量 | 整体替换 |
| 列表 | 整体替换（不按下标合并） |
| 显式 `null` / `~` | **删除**基础层对应的键 |

```yaml
# values.yaml（第 1 层）
app:
  port: 8080
  debug: true
  tags: [a, b]

# -f override.yaml（第 2 层）
app:
  port: 9090       # 覆盖
  debug: null      # 删除 debug 键
  tags: [c]        # 整体替换为 [c]
```

## --set 点路径语法

```sh
--set app.port=9090             # 标量
--set app.tags[0]=web           # 列表下标（单层）
--set app.feature.enabled=true  # 自动创建中间 map
--set app.debug=false           # 布尔
--set app.timeout=30            # 整数
--set app.old=null              # 删除键
```

类型自动推断：`true`/`false` → 布尔，`null`/`~` → 删除，整数/浮点 → 数值，其余 → 字符串。

限制（已知）：多级下标 `a[0][1]` 暂不支持；需要复杂结构时用 `-f` 文件。

## 模板引擎

Go text/template，`missingkey=error`（**未定义变量直接报错**，尽早暴露拼写问题）。

### 基本语法

```yaml
{{ .app.port }}                    # 变量引用
{{ if eq .os.family "debian" }}…{{ end }}
{{ range .groups.webservers }}…{{ end }}
{{ index .hosts "web2" }}          # map 动态键
```

### 可选键的正确写法

严格模式下引用不存在的键会报错。**可选键用 sprig 的 `dig`**（缺键时返回默认值）；`default` 只处理"值为空"（nil/空串），不处理"键不存在"：

```yaml
{{ dig "mirror" "builtin" . }}              # .mirror 不存在 → "builtin"
{{ .app.name | default "unnamed" }}         # .app.name 存在但为空 → "unnamed"
```

## 模板函数全集

### Go 内建

`eq ne lt le gt ge and or not len index slice print printf println html js urlquery call`

### sprig v3（白名单，Helm 同款常用集）

以下为模板可用的 sprig 函数全集（白名单机制：仅表内函数与 Go 内建、wdp 自有函数可用）：

| 类别 | 函数 |
|---|---|
| 字典/列表 | `dict` `list` `concat` `append` `prepend` `first` `last` `rest` `initial` `reverse` `uniq` `without` `has` `compact` `dig` `keys` `pick` `omit` `merge` `values` |
| 字符串 | `trim` `trimAll` `trimPrefix` `trimSuffix` `upper` `lower` `title` `untitle` `repeat` `substr` `trunc` `abbrev` `initials` `randAlpha` `randAlphaNum` `randNumeric` `wrap` `contains` `hasPrefix` `hasSuffix` `quote` `squote` `cat` `indent` `nindent` `replace` `plural` `sha1sum` `sha256sum` `adler32sum` `toString` `atoi` `int64` `int` `float64` `seq` `toDecimal` |
| 正则 | `regexMatch` `regexFindAll` `regexFind` `regexReplaceAll` `regexReplaceAllLiteral` `regexSplit` |
| 类型转换 | `toJson` `fromJson` `toPrettyJson` `ternary` `default` `empty` `coalesce` `all` `any` `kindOf` `typeOf` `kindIs` `typeIs` |
| 数学 | `add` `sub` `mul` `div` `mod` `max` `min` `ceil` `floor` `round` `add1` |
| 日期 | `now` `date` `dateInZone` `dateModify` `duration` `unixEpoch` `htmlDate` `toDate` |
| 编码 | `b64enc` `b64dec` `b32enc` `b32dec` `toJson` |
| 流程 | `fail` `uuidv4` `semver` `semverCompare` `regexQuoteMeta` |

> **安全说明**：模板函数走白名单而非"全集减黑名单"，以下函数不可用：
> - `env` / `expandenv`——chart 模板不得读取控制端环境变量（其中可能含
>   `WDP_CA_PASSPHRASE` 与各类 `*_env` 密钥）；`getHostByName` 同理
>   （DNS 查询可被用作隐蔽外传信道）。
> - 证书/密钥生成与凭据散列原语——`genPrivateKey` / `genCA` / `genSignedCert` /
>   `genSelfSignedCert` / `buildCustomCertificate` / `encryptAES` / `decryptAES` /
>   `bcrypt` / `htpasswd` / `derivePassword` 等。部署模板没有正当理由使用它们，
>   而它们对不可信 chart 模板是真实的 CPU 滥用与密码学误用面。
>
> 注：YAML/TOML 互转不在 sprig 中（Helm 由引擎层提供）；wdp 引擎自带下述
> `to_yaml` / `from_yaml`，配置文件嵌套首选。

### wdp 自有函数（覆盖 sprig 同名，保持旧 chart 兼容）

| 函数 | 签名 | 说明 |
|---|---|---|
| `default` | `default def v` | v 为 nil/空串时返回 def |
| `upper` / `lower` / `trim` | `string → string` | 大写/小写/去空白 |
| `quote` | `any → string` | 加双引号 |
| `b64enc` / `b64dec` | `any → string` | base64 编解码 |
| `split` | `split sep s → []string` | 按分隔符切分 |
| `join` | `join list sep → string` | 连接（注意参数序：先列表后分隔符） |
| `replace` | `replace old new s` | 全量替换 |
| `contains` / `hasPrefix` / `hasSuffix` | `→ bool` | 子串判断 |
| `to_json` | `any → string` | JSON 序列化 |
| `to_yaml` | `any → string` | YAML 序列化（嵌配置文件首选） |
| `from_yaml` | `string → map` | YAML 反序列化 |
| `include` | `include "名字" . → string` | 执行命名模板（可参与管道） |

### 常用组合示例

```yaml
# 生成 labels 块（缩进 2）
labels:
{{ to_yaml (dict "app" .app.name "env" .global.env) | nindent 2 }}

# 三元选择
mode: {{ ternary "cluster" "single" (ge .app.replicas 2) }}

# 生成随机密码（写配置）
password: {{ randAlphaNum 24 | quote }}

# 证书剩余天数判断
{{ if lt (now | unixEpoch) 1700000000 }}…{{ end }}

# 版本比较（子 chart 引用约束也可用 semverCompare 校验 values）
{{ if semverCompare "^1.2" .app.version }}…{{ end }}
```

## 渲染时机

- 任务参数、free-form、when/until/changed_when/failed_when、environment、delegate_to：每主机每 item 渲染
- `templates/` 配置模板：template 模块执行时按该主机变量域渲染
- `loop`：渲染结果须为列表
- 未含 `{{` 的字符串跳过渲染（零开销快速路径）
