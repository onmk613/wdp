package shellquote

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"abc", "'abc'"},
		{"a b c", "'a b c'"},
		{"it's", `'it'\''s'`},
		{"a'b\"c", `'a'\''b"c'`},
		{"$(rm -rf /)", `'$(rm -rf /)'`},
		{"`id`", "'`id`'"},
		{"a\nb", "'a\nb'"},
		{"back\\slash", `'back\slash'`},
		{"中文", "'中文'"},
		{"semi;colon|pipe&amp", "'semi;colon|pipe&amp'"},
	}
	for _, c := range cases {
		if got := Quote(c.in); got != c.want {
			t.Errorf("Quote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestQuoteShellRoundTrip 用真实 /bin/sh 验证转义后的字面量与原文完全一致
// （注入载体：引号、命令替换、反引号、分号、管道均不改变字面量语义）。
func TestQuoteShellRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /bin/sh on windows")
	}
	inputs := []string{
		"",
		"plain",
		"it's a 'quoted' string",
		"$(id) `id` ${HOME}",
		"a;rm -rf /|b&c",
		"line1\nline2",
		"back\\slash",
		strings.Repeat("'", 10),
		strings.Repeat("a'", 50),
	}
	for _, in := range inputs {
		got, err := exec.Command("/bin/sh", "-c", "printf %s "+Quote(in)).Output()
		if err != nil {
			t.Fatalf("sh 执行失败（input=%q）: %v", in, err)
		}
		// printf %s 不追加换行；sh 输出与原文逐字节一致才证明无注入、无损转义
		if string(got) != in {
			t.Errorf("round trip 失败: input=%q got=%q", in, string(got))
		}
	}
}

func TestSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  \t\n ", nil},
		{"a b c", []string{"a", "b", "c"}},
		{`--flag "two words"`, []string{"--flag", "two words"}},
		{`'single quoted' x`, []string{"single quoted", "x"}},
		{`a\ b`, []string{"a b"}},
		{`pre"mid"dle`, []string{"premiddle"}}, // 引号段与裸段拼接为同一词
		{`"it\"s"`, []string{`it"s`}},
		{`"a\b"`, []string{`a\b`}}, // 双引号内 \b 非特殊，保持字面
		{`'a\nb'`, []string{`a\nb`}},
		{`$HOME ;|&&`, []string{"$HOME", ";|&&"}},
	}
	for _, c := range cases {
		got, err := Split(c.in)
		if err != nil {
			t.Errorf("Split(%q) 意外出错: %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("Split(%q) = %q, want %q", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("Split(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestSplitErrors(t *testing.T) {
	for _, in := range []string{`'unclosed`, `"unclosed`, `trailing\`} {
		if _, err := Split(in); err == nil {
			t.Errorf("Split(%q) 应报错（未闭合）", in)
		}
	}
}

// TestQuoteWordsShellRoundTrip 真实 /bin/sh 验证：free-form 参数经 QuoteWords
// 后每个元字符词都字面传递，命令替换/反引号不被解释。
func TestQuoteWordsShellRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /bin/sh on windows")
	}
	quoted, err := QuoteWords(`--tag "a b" $(id) ` + "`id`" + ` ;x`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", "-c", "printf %s "+quoted).Output()
	if err != nil {
		t.Fatalf("sh 执行失败: %v", err)
	}
	want := "--taga b$(id)`id`;x" // printf %s 逐参数复用格式，各词直接相连
	if string(out) != want {
		t.Errorf("QuoteWords round trip 失败: got=%q want=%q", string(out), want)
	}
}
