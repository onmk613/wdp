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
		"fr":      Auto,
		"":        Auto,
	}
	for in, want := range cases {
		if got := normalize(Lang(in)); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestT(t *testing.T) {
	defer func() { lang = En }()
	lang = En
	if got := T("hello", "你好"); got != "hello" {
		t.Errorf("en: got %q", got)
	}
	lang = Zh
	if got := T("hello", "你好"); got != "你好" {
		t.Errorf("zh: got %q", got)
	}
}

func TestIsChinaRegion(t *testing.T) {
	oldLANG := os.Getenv("LANG")
	oldLocal := time.Local
	defer func() {
		os.Setenv("LANG", oldLANG)
		time.Local = oldLocal
	}()

	os.Setenv("LANG", "en_US.UTF-8")

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
	os.Setenv("LANG", "zh_CN.UTF-8")
	time.Local = time.FixedZone("Asia/Singapore", 8*3600)
	if !isChinaRegion() {
		t.Error("Asia/Singapore + zh_CN locale should be China region")
	}
}

func TestHasZhLocale(t *testing.T) {
	oldLANG := os.Getenv("LANG")
	oldLCAll := os.Getenv("LC_ALL")
	oldLCCType := os.Getenv("LC_CTYPE")
	defer func() {
		os.Setenv("LANG", oldLANG)
		os.Setenv("LC_ALL", oldLCAll)
		os.Setenv("LC_CTYPE", oldLCCType)
	}()

	os.Setenv("LC_ALL", "")
	os.Setenv("LC_CTYPE", "")
	os.Setenv("LANG", "en_US.UTF-8")
	if hasZhLocale() {
		t.Error("en_US.UTF-8 should not be zh locale")
	}
	os.Setenv("LANG", "zh_CN.UTF-8")
	if !hasZhLocale() {
		t.Error("zh_CN.UTF-8 should be zh locale")
	}
	os.Setenv("LANG", "C")
	os.Setenv("LC_CTYPE", "zh_TW.UTF-8")
	if !hasZhLocale() {
		t.Error("LC_CTYPE=zh_TW.UTF-8 should be zh locale")
	}
	os.Setenv("LC_CTYPE", "")
	os.Setenv("LANG", "中文")
	if !hasZhLocale() {
		t.Error("LANG=中文 should be zh locale")
	}
}

func TestIsUTF8Terminal(t *testing.T) {
	// 隔离 locale 优先级链（LC_ALL > LC_CTYPE > LANG），避免宿主机环境干扰断言
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

func TestResolve(t *testing.T) {
	defer func() { lang = En }()
	if got := Resolve("zh"); got != Zh {
		t.Errorf("Resolve(zh) = %q", got)
	}
	if got := Resolve("en"); got != En {
		t.Errorf("Resolve(en) = %q", got)
	}
}
