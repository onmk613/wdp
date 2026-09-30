// Package i18n 承载 CLI 与模块文档的中英文双语判定。
//
// 设计取向（刻意从简）：不做 message catalog、不做占位符插值，只在调用点
// 把 en/zh 两份文案成对写出，由 T 按当前语言返回其一。这样文案就长在它被
// 使用的地方，改一处不会漏掉另一处；代价是每处多写一份字符串，换来的是
// 没有 key 与文案漂移、没有加载失败、没有运行时查表。
//
// 语言只由环境变量决定，且在 init() 里一次性定死：没有命令行标志、没有
// 配置文件项、没有"每次取用再判一次"的分支。判定优先级
// WDP_LANG > CLI_LANG > 自动检测（中国时区 + 中文编码支持）。CLI_LANG 是与
// s3cli 共存时的兼容位（同机两个工具共用一份偏好）。
//
// 缺省语言为 Zh——本项目文案原生中文，未表态的用户看到中文才是不变的行为
// （英文需显式 WDP_LANG=en）。
package i18n

import (
	"os"
	"strings"
	"sync"
	"time"
)

// Lang 表示输出语言。
type Lang string

const (
	// En 表示英文。
	En Lang = "en"
	// Zh 表示中文。
	Zh Lang = "zh"
)

var (
	mu   sync.Mutex
	lang Lang
)

// init 是唯一的语言判定点：进程启动即定死，之后 T 只读不判。
// 放在 init 而非首次 T 调用，是为了让所有文案（含包级 init 期就构造好的
// 命令树）拿到一致的语言，且热路径上不再碰环境变量。
func init() { lang = Resolve() }

// T 按当前语言返回对应内容。en/zh 两个文案必须在同一调用点成对给出。
func T(en, zh string) string {
	mu.Lock()
	cur := lang
	mu.Unlock()
	if cur == Zh {
		return zh
	}
	return en
}

// Current 返回当前生效语言（排障用）。
func Current() Lang {
	mu.Lock()
	defer mu.Unlock()
	return lang
}

// Resolve 按环境变量解析语言并全局生效，返回结果。init() 调用它；测试也
// 用它显式指定语言（免去在每个用例里摆弄 TZ/LANG 环境）。
// 优先级：WDP_LANG > CLI_LANG > 自动检测。
func Resolve() Lang {
	mu.Lock()
	defer mu.Unlock()
	switch normalize(pref()) {
	case Zh:
		lang = Zh
	case En:
		lang = En
	default:
		if isChinaRegion() && isUTF8Terminal() {
			lang = Zh
		} else {
			lang = En
		}
	}
	return lang
}

// pref 返回环境变量里的语言偏好，未设置返回空串（走自动检测）。
func pref() string {
	if v := os.Getenv("WDP_LANG"); v != "" {
		return v
	}
	return os.Getenv("CLI_LANG")
}

// normalize 把常见写法归一为 zh/en，其余返回 auto（空值）。
func normalize(v string) Lang {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "zh", "zh_cn", "zh-cn", "zhcn", "chinese", "中文":
		return Zh
	case "en", "en_us", "en-us", "english", "英文":
		return En
	default:
		return ""
	}
}

// chinaTimeZones 是中国地区的 IANA 时区名。
var chinaTimeZones = map[string]bool{
	"Asia/Shanghai":  true,
	"Asia/Chongqing": true,
	"Asia/Harbin":    true,
	"Asia/Urumqi":    true,
	"Asia/Hong_Kong": true,
	"Asia/Macau":     true,
	"Asia/Taipei":    true,
}

// ambiguousChinaZoneNames 是 Windows 中文系统的中国时区显示名, 语义有歧义:
// "CST" 在 Unix 上同时是 America/Chicago 冬令时 (UTC-6) 的缩写。
// 若无条件命中, 美国中部用户冬季会被误判为中国时区 (夏季 CDT 又恢复),
// 随季节变化的错判极难排查 —— 因此仅当偏移恰为 UTC+8 时才视为中国时区。
var ambiguousChinaZoneNames = map[string]bool{
	"CST":                 true,
	"China Standard Time": true,
}

// isChinaRegion 检测当前系统时区与 locale 是否指向中国地区：
//   - 时区名命中 chinaTimeZones（IANA 名）→ 是
//   - 歧义名（CST 等）且偏移为 UTC+8 → 是
//   - 否则偏移为 UTC+8 且时区名为空（POSIX 环境固定偏移的兜底）→ 是
//   - 否则偏移为 UTC+8 且 locale 含中文标志（zh_CN/zh/中文）→ 是
//   - 其余情况（如 Asia/Singapore + en_US）→ 否
func isChinaRegion() bool {
	name, offset := time.Now().Zone()
	if chinaTimeZones[name] {
		return true
	}
	if offset != 8*3600 {
		return false
	}
	if ambiguousChinaZoneNames[name] || name == "" {
		return true
	}
	return hasZhLocale()
}

// hasZhLocale 检查 locale 环境变量（LC_ALL/LC_CTYPE/LANG）是否含中文标志
// （zh_CN / zh / 中文），用于 UTC+8 但时区名非中国时区时的兜底判断。
func hasZhLocale() bool {
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(key); v != "" {
			u := strings.ToLower(v)
			if strings.Contains(u, "zh") || strings.Contains(u, "中文") {
				return true
			}
		}
	}
	return false
}

// isUTF8Terminal 检测当前终端环境是否支持中文编码：
// 优先看 locale 环境变量（UTF-8 / zh_* / GBK / GB2312），
// Windows 下再补充控制台代码页检查（65001）。
func isUTF8Terminal() bool {
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(key); v != "" {
			u := strings.ToUpper(v)
			if strings.Contains(u, "UTF-8") || strings.Contains(u, "UTF8") ||
				strings.Contains(u, "ZH") || strings.Contains(u, "GBK") ||
				strings.Contains(u, "GB2312") || strings.Contains(u, "GB18030") {
				return true
			}
		}
	}
	return consoleUTF8()
}
