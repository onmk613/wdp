package fmtutil

import (
	"strings"
	"unicode/utf8"
)

// TruncateUTF8 按字节截断到 max 长度并回退到完整 UTF-8 序列边界：截断点
// 落在多字节字符（任务输出常见中文）中间时丢掉该不完整字符，避免输出
// 尾部出现残缺字节（终端显示为乱码）。
//
// 判定依据是 s[cut]（第一个被丢弃的字节）：续字节（0b10xxxxxx，RuneStart
// 为 false）说明 cut 落在字符中间，持续回退直到 cut 位于字符边界。按
// s[cut-1]（最后一个保留的字节）判定是错的——保留串恰以完整多字节字符
// 结尾时会误退丢字符，而 cut 落在字符中间时又会停在 lead 字节之后留下
// 残缺字节。非法序列原样保留（截断宽容优于报错）。
func TruncateUTF8(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if max >= len(s) {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// FirstLine 返回 s 的首行并去首尾空白：多行错误信息的摘要用（表格单元
// 格、告警文案、巡检结论行）。空串与纯空白串返回空串。
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// FirstLineOr 返回 s 的首行（去首尾空白，同 FirstLine），空结果时返回
// def 兜底：巡检/探测结论对空信息要给出确定性文案而非空串。
func FirstLineOr(s, def string) string {
	if fl := FirstLine(s); fl != "" {
		return fl
	}
	return def
}
