package render

// sprigAllowlist 是暴露给模板的 sprig 函数白名单——即 docs/09「模板函数全集」
// 速查表承诺的函数集合。
//
// 为什么是白名单而不是"全集减黑名单"：sprig 全集含 genPrivateKey/genCA/
// genSignedCert/buildCustomCertificate/encryptAES/decryptAES/bcrypt/htpasswd/
// derivePassword 等证书·密钥生成与凭据散列原语，对不可信 chart 模板是真实的
// CPU/滥用攻击面，且部署模板没有正当理由使用它们。这些函数从未出现在文档
// 速查表中，白名单将其与其它未文档化函数一并挡在模板之外。
//
// 维护约定：向速查表新增函数时必须同步本清单（单一事实来源仍是文档表格，
// 本清单是其代码侧投影）；测试 TestSprigAllowlistCoversDocs 锁定两者一致。
var sprigAllowlist = []string{
	// 字典/列表
	"dict", "list", "concat", "append", "prepend", "first", "last", "rest",
	"initial", "reverse", "uniq", "without", "has", "compact", "dig",
	"keys", "pick", "omit", "merge", "values",
	// 字符串
	"trim", "trimAll", "trimPrefix", "trimSuffix", "upper", "lower", "title",
	"untitle", "repeat", "substr", "trunc", "abbrev", "initials",
	"randAlpha", "randAlphaNum", "randNumeric",
	"wrap", "contains", "hasPrefix", "hasSuffix", "quote", "squote", "cat",
	"indent", "nindent", "replace", "plural",
	"sha1sum", "sha256sum", "adler32sum",
	"toString", "atoi", "int64", "int", "float64", "seq", "toDecimal",
	// 正则
	"regexMatch", "regexFindAll", "regexFind", "regexReplaceAll",
	"regexReplaceAllLiteral", "regexSplit", "regexQuoteMeta",
	// 类型与判定
	"toJson", "fromJson", "toPrettyJson", "ternary", "default", "empty",
	"coalesce", "all", "any", "kindOf", "typeOf", "kindIs", "typeIs",
	// 数学
	"add", "sub", "mul", "div", "mod", "max", "min", "ceil", "floor", "round", "add1",
	// 日期
	"now", "date", "dateInZone", "dateModify", "duration", "unixEpoch", "htmlDate", "toDate",
	// 编码
	// 注：docs 曾把 toToml/toYaml 列为 sprig 函数，实为 Helm 引擎侧提供，
	// wdp 引擎从未有过（YAML 互转用 wdp 自有 to_yaml/from_yaml）
	"b64enc", "b64dec", "b32enc", "b32dec",
	// 流程与其他
	"fail", "uuidv4", "semver", "semverCompare",
}
