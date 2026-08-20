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
	// Auto 表示自动检测（中国时区 + 中文编码支持 → 中文）。
	Auto Lang = "auto"
	// En 表示英文。
	En Lang = "en"
	// Zh 表示中文。
	Zh Lang = "zh"
)

var (
	mu   sync.Mutex
	lang Lang = En
)

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

// Resolve 根据用户偏好解析语言并全局生效：
// zh/en 直接采用，auto/空 则按「中国时区 + 中文编码支持」自动检测。
func Resolve(pref string) Lang {
	switch normalize(Lang(pref)) {
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

// normalize 把常见写法归一为 zh/en，其余返回 auto。
func normalize(l Lang) Lang {
	switch strings.ToLower(strings.TrimSpace(string(l))) {
	case "zh", "zh_cn", "zh-cn", "zhcn", "chinese", "中文":
		return Zh
	case "en", "en_us", "en-us", "english", "英文":
		return En
	default:
		return Auto
	}
}

// chinaTimeZones 是中国地区的 IANA 时区名。
// 部分平台（如 Windows 中文系统）把中国标准时间记为 CST / China Standard Time。
var chinaTimeZones = map[string]bool{
	"Asia/Shanghai":       true,
	"Asia/Chongqing":      true,
	"Asia/Harbin":         true,
	"Asia/Urumqi":         true,
	"Asia/Hong_Kong":      true,
	"Asia/Macau":          true,
	"Asia/Taipei":         true,
	"CST":                 true,
	"China Standard Time": true,
}

// isChinaRegion 检测当前系统时区与 locale 是否指向中国地区：
//   - 时区名命中 chinaTimeZones（含 Windows 的 "China Standard Time"）→ 是
//   - 否则偏移为 UTC+8 且时区名为空（POSIX 环境固定偏移的兜底）→ 是
//   - 否则偏移为 UTC+8 且 locale 含中文标志（zh_CN/zh/中文）→ 是
//   - 其余情况（如 Asia/Singapore + en_US）→ 否
func isChinaRegion() bool {
	now := time.Now()
	name, offset := now.Zone()
	if chinaTimeZones[name] {
		return true
	}
	if offset != 8*3600 {
		return false
	}
	if name == "" {
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
