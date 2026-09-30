package i18n

import (
	"os"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	cases := map[string]Lang{
		"zh":      Zh,
		"zh_CN":   Zh,
		"zh-cn":   Zh,
		"中文":      Zh,
		"en":      En,
		"EN_US":   En,
		"English": En,
		"fr":      "",
		"":        "",
	}
	for in, want := range cases {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestT(t *testing.T) {
	old := Current()
	t.Cleanup(func() { Resolve(); _ = old })

	t.Setenv("WDP_LANG", "en")
	Resolve()
	if got := T("hello", "你好"); got != "hello" {
		t.Errorf("en: got %q", got)
	}
	t.Setenv("WDP_LANG", "zh")
	Resolve()
	if got := T("hello", "你好"); got != "你好" {
		t.Errorf("zh: got %q", got)
	}
}

// TestPref 钉住环境变量优先级 WDP_LANG > CLI_LANG，以及未设置时为空串
// （空串即走自动检测）。
func TestPref(t *testing.T) {
	t.Setenv("WDP_LANG", "")
	t.Setenv("CLI_LANG", "")
	if got := pref(); got != "" {
		t.Errorf("无环境变量时 pref() = %q, want 空串", got)
	}
	t.Setenv("CLI_LANG", "en")
	if got := pref(); got != "en" {
		t.Errorf("CLI_LANG=en 时 pref() = %q, want en", got)
	}
	t.Setenv("WDP_LANG", "zh")
	if got := pref(); got != "zh" {
		t.Errorf("WDP_LANG 应压过 CLI_LANG, pref() = %q, want zh", got)
	}
}

// TestResolveFromEnv 覆盖 Resolve 的完整优先级：显式 zh/en 直接采用，
// 未表态才落到自动检测。
func TestResolveFromEnv(t *testing.T) {
	t.Cleanup(func() { Resolve() })

	t.Setenv("CLI_LANG", "en")
	t.Setenv("WDP_LANG", "中文")
	if got := Resolve(); got != Zh {
		t.Errorf("WDP_LANG=中文 → %q, want zh", got)
	}
	t.Setenv("WDP_LANG", "English")
	if got := Resolve(); got != En {
		t.Errorf("WDP_LANG=English → %q, want en", got)
	}
	// 未表态：按当前时区/locale 自动检测，只要是个确定值即可
	t.Setenv("WDP_LANG", "")
	t.Setenv("CLI_LANG", "")
	if got := Resolve(); got != Zh && got != En {
		t.Errorf("自动检测返回了非 zh/en 的值: %q", got)
	}
}

func TestIsChinaRegion(t *testing.T) {
	oldLocal := time.Local
	t.Cleanup(func() {
		time.Local = oldLocal
	})

	t.Setenv("LANG", "en_US.UTF-8")

	// UTC+8 固定偏移, 时区名命中 chinaTimeZones (含 Windows "China Standard Time")
	time.Local = time.FixedZone("CST", 8*3600)
	if !isChinaRegion() {
		t.Error("expect China region with UTC+8 zone name CST")
	}
	time.Local = time.FixedZone("China Standard Time", 8*3600)
	if !isChinaRegion() {
		t.Error("expect China region with Windows zone name")
	}
	time.Local = time.FixedZone("Asia/Shanghai", 8*3600)
	if !isChinaRegion() {
		t.Error("expect China region by zone name")
	}

	// 回归: America/Chicago 冬令时的缩写恰为 "CST" (Central Standard Time, UTC-6),
	// 不得因名字撞车被误判为中国时区 (用户会突然看到中文帮助, 夏季又恢复英文)。
	time.Local = time.FixedZone("CST", -6*3600)
	if isChinaRegion() {
		t.Error("CST at UTC-6 (US Central) must NOT be China region")
	}

	// UTC+8 但时区名非中国 (新加坡) + en locale -> 不判定为中国
	time.Local = time.FixedZone("Asia/Singapore", 8*3600)
	if isChinaRegion() {
		t.Error("Asia/Singapore + en_US should NOT be China region")
	}

	// UTC+8 且时区名为空 -> 保留兜底判定
	time.Local = time.FixedZone("", 8*3600)
	if !isChinaRegion() {
		t.Error("expect China region with empty zone name and UTC+8")
	}

	// UTC-5 -> 非中国
	time.Local = time.FixedZone("EST", -5*3600)
	if isChinaRegion() {
		t.Error("expect non-China region with UTC-5")
	}

	// UTC+8 时区名非中国, 但 locale 含中文标志 -> 判定为中国
	t.Setenv("LANG", "zh_CN.UTF-8")
	time.Local = time.FixedZone("Asia/Singapore", 8*3600)
	if !isChinaRegion() {
		t.Error("Asia/Singapore + zh_CN locale should be China region")
	}
}

func TestHasZhLocale(t *testing.T) {
	// t.Setenv 保证三个 locale 变量被完全钉住: 实现按 LC_ALL > LC_CTYPE > LANG
	// 优先级检测, 宿主机 (如 macOS 终端默认导出 LC_CTYPE) 的残留值会污染测试。
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")
	t.Setenv("LANG", "en_US.UTF-8")
	if hasZhLocale() {
		t.Error("en_US.UTF-8 should not be zh locale")
	}
	t.Setenv("LANG", "zh_CN.UTF-8")
	if !hasZhLocale() {
		t.Error("zh_CN.UTF-8 should be zh locale")
	}
	t.Setenv("LANG", "C")
	t.Setenv("LC_CTYPE", "zh_TW.UTF-8")
	if !hasZhLocale() {
		t.Error("LC_CTYPE=zh_TW.UTF-8 should be zh locale")
	}
	t.Setenv("LC_CTYPE", "")
	t.Setenv("LANG", "中文")
	if !hasZhLocale() {
		t.Error("LANG=中文 should be zh locale")
	}
}

func TestIsUTF8Terminal(t *testing.T) {
	// 同上: 必须清空 LC_ALL/LC_CTYPE, 只用 LANG 驱动断言,
	// 否则 macOS 宿主机的 LC_CTYPE=C.UTF-8 会让 "C locale" 分支误报。
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")
	t.Setenv("LANG", "zh_CN.UTF-8")
	if !isUTF8Terminal() {
		t.Error("zh_CN.UTF-8 should be treated as UTF-8 capable")
	}
	t.Setenv("LANG", "C")
	if isUTF8Terminal() {
		t.Error("C locale should not be treated as UTF-8 capable")
	}
}

// TestInitResolved 断言 init 已经把语言定死（不是零值空串）：T 在任何调用
// 点都必须能直接工作，不依赖谁先调 Resolve。
func TestInitResolved(t *testing.T) {
	if got := Current(); got != Zh && got != En {
		t.Fatalf("init 后 Current() = %q，应为 zh 或 en", got)
	}
	if got := T("en-text", "zh-text"); got != "en-text" && got != "zh-text" {
		t.Fatalf("T 返回了未预期的值 %q", got)
	}
	_ = os.Getenv // 保持 os 导入（其余用例用 t.Setenv）
}
