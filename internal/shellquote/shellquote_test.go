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
