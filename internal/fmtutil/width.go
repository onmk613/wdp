package fmtutil

import (
	"regexp"
	"strings"

	"github.com/mattn/go-runewidth"
)

// ansiRe 匹配 ANSI 转义序列（CSI），用于宽度计算前剥离着色码。
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// widthCond 固定 EastAsianWidth=false，不随 EastAsianWidth 环境变量
// 与终端 locale 漂移（包级默认函数会做环境探测），保证任意环境下
// 表格列宽计算确定一致。
var widthCond = runewidth.Condition{EastAsianWidth: false}

// displayWidth 计算字符串的终端显示宽度：CJK 宽字符按 2 计，组合符号
// 按 0 计，ZWJ emoji/国旗等多 rune 序列按 grapheme 聚合为单格并封顶
// 2 列（go-runewidth 语义）；混入的 ANSI 颜色码不占宽度。
func displayWidth(s string) int {
	if strings.IndexByte(s, 0x1b) >= 0 {
		s = ansiRe.ReplaceAllString(s, "")
	}
	return widthCond.StringWidth(s)
}

// DisplayWidth 计算字符串的终端显示宽度，语义与内部 displayWidth 相同，
// 供包外复用（如截断长消息时按显示宽度计算）。
func DisplayWidth(s string) int { return displayWidth(s) }

// TruncateDisplay 按终端显示宽度截断字符串（附加省略号 …），
// 用于表格单元格等定宽场景; s 已短于 max 时原样返回。
// 截断按 grapheme 边界进行，不会撕开组合符号与 emoji 序列。
func TruncateDisplay(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if displayWidth(s) <= max {
		return s
	}
	return widthCond.Truncate(s, max, "…")
}
