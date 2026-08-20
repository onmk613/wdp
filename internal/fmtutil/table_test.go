package fmtutil

import (
	"bytes"
	"strings"
	"testing"
)

func renderTable(t *testing.T, tb *Table) string {
	t.Helper()
	var buf bytes.Buffer
	tb.To(&buf)
	tb.Render()
	return buf.String()
}

func TestTableAlignsColumns(t *testing.T) {
	tb := NewTable("Time", "Size", "Type", "Path").AlignRight(1)
	tb.AddRow(C("2026-08-10 11:00:00"), C("1048576"), C("FILE"), C("minio:bucket/a.txt"))
	tb.AddRow(C("-"), C("-"), C("DIR"), C("minio:bucket/dir/"))
	out := renderTable(t, tb)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines (header/sep/2 rows), got %d:\n%s", len(lines), out)
	}
	// 数字列右对齐
	if !strings.Contains(lines[2], "  1048576  FILE") {
		t.Errorf("size column should be right aligned:\n%s", out)
	}
	// 路径为末列, 不补尾空格
	if !strings.HasSuffix(lines[2], "minio:bucket/a.txt") || !strings.HasSuffix(lines[3], "minio:bucket/dir/") {
		t.Errorf("path column unexpected:\n%s", out)
	}
}

func TestTableCJKWidth(t *testing.T) {
	tb := NewTable("键", "值")
	tb.AddRow(C("存储桶"), C("v1"))
	tb.AddRow(C("b"), C("v2"))
	out := renderTable(t, tb)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// 中文表头 "键" 占 2 列宽, "b" 只占 1; 分隔线须等长且第 2 列起点一致
	sep := lines[1]
	if !strings.HasPrefix(sep, strings.Repeat("-", displayWidth("键"))) {
		t.Errorf("separator width should account CJK width:\n%s", out)
	}
	row1, row2 := lines[2], lines[3]
	idx1 := displayWidth(row1[:strings.Index(row1, "v1")])
	idx2 := displayWidth(row2[:strings.Index(row2, "v2")])
	if idx1 != idx2 {
		t.Errorf("value column misaligned with CJK cells (%d vs %d):\n%s", idx1, idx2, out)
	}
}

func TestTableEmpty(t *testing.T) {
	tb := NewTable("A", "B")
	if out := renderTable(t, tb); out != "" {
		t.Errorf("empty table should render nothing, got %q", out)
	}
}

func TestTableNoHeader(t *testing.T) {
	// 无表头时不 panic, 列数按数据行推断
	tb := NewTable().AlignRight(0)
	tb.AddRow(C("Version:"), C("v1.2.3"))
	tb.AddRow(C("Commit:"), C("abc1234"))
	out := renderTable(t, tb)
	if !strings.Contains(out, "Version:") || !strings.Contains(out, "abc1234") {
		t.Errorf("no-header table output wrong:\n%s", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d:\n%s", len(lines), out)
	}
	// 第 0 列右对齐: "Version:" 与 "  Commit:" 同宽
	if displayWidth(lines[0][:strings.Index(lines[0], "v1.2.3")]) !=
		displayWidth(lines[1][:strings.Index(lines[1], "abc1234")]) {
		t.Errorf("first column not aligned:\n%s", out)
	}
}

func TestTableLastColumnAlignRight(t *testing.T) {
	tb := NewTable("Name", "Count").AlignRight(1)
	tb.AddRow(C("a"), C("9"))
	tb.AddRow(C("bb"), C("100"))
	out := renderTable(t, tb)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// 右对齐: 数字列的结束位置对齐
	end1 := displayWidth(lines[2][:strings.Index(lines[2], "9")+1])
	end2 := displayWidth(lines[3][:strings.Index(lines[3], "100")+3])
	if end1 != end2 {
		t.Errorf("last column should right align:\n%s", out)
	}
}

func TestTableWiderRowsThanHeader(t *testing.T) {
	// 数据行列数超过表头时不丢列
	tb := NewTable("A")
	tb.AddRow(C("1"), C("2"), C("3"))
	out := renderTable(t, tb)
	if !strings.Contains(out, "2") || !strings.Contains(out, "3") {
		t.Errorf("extra cells dropped:\n%s", out)
	}
}

func TestDisplayWidthStripsANSI(t *testing.T) {
	if got := displayWidth("\x1b[32mgreen\x1b[0m"); got != 5 {
		t.Errorf("displayWidth with ANSI = %d, want 5", got)
	}
	if got := displayWidth("中\x1b[1;34m文\x1b[0m"); got != 4 {
		t.Errorf("displayWidth with ANSI+CJK = %d, want 4", got)
	}
}

func TestTruncateDisplay(t *testing.T) {
	if got := TruncateDisplay("abc", 5); got != "abc" {
		t.Errorf("short string should pass through: %q", got)
	}
	if got := TruncateDisplay("abcdefgh", 5); got != "abcd…" {
		t.Errorf("TruncateDisplay = %q, want abcd…", got)
	}
	// CJK 按显示宽度计: 2 个中文 = 4 列
	if got := TruncateDisplay("中文混排abc", 7); got != "中文混…" {
		t.Errorf("TruncateDisplay CJK = %q, want 中文混…", got)
	}
}

func TestTableViaPrinterColor(t *testing.T) {
	// 绑定 Printer 的表格: 开色时单元格/表头带 ANSI, 关色时纯文本
	var buf bytes.Buffer
	p := New()
	p.SetWriter(&buf)

	p.SetColor(true)
	tb := p.NewTable("H", "S")
	tb.AddRow(CC("x", Red), C("y"))
	tb.Render()
	if !strings.Contains(buf.String(), "\x1b[31m") || !strings.Contains(buf.String(), "\x1b[1;34m") {
		t.Errorf("colored table missing ANSI codes:\n%q", buf.String())
	}

	buf.Reset()
	p.SetColor(false)
	tb2 := p.NewTable("H", "S")
	tb2.AddRow(CC("x", Red), C("y"))
	tb2.Render()
	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("no-color printer should strip ANSI:\n%q", buf.String())
	}
	if !strings.Contains(buf.String(), "x") || !strings.Contains(buf.String(), "y") {
		t.Errorf("table content wrong:\n%q", buf.String())
	}
}

func TestRenderToPlain(t *testing.T) {
	tb := NewTable("A", "B")
	tb.AddRow(CC("x", Green), C("y"))
	var buf bytes.Buffer
	tb.RenderTo(&buf)
	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("RenderTo should not contain ANSI codes:\n%q", buf.String())
	}
	if !strings.Contains(buf.String(), "x") || !strings.Contains(buf.String(), "y") {
		t.Errorf("RenderTo output wrong:\n%s", buf.String())
	}
}

func TestTablePlainRowLimit(t *testing.T) {
	// 超过上限后切换为 TSV 流式输出: 首行表头, 制表符分隔, 无对齐
	tb := NewTable("A", "B").PlainRowLimit(2)
	var buf bytes.Buffer
	tb.To(&buf)

	tb.AddRow(C("1"), C("x"))
	tb.AddRow(C("22"), C("y"))
	tb.AddRow(C("333"), C("z")) // 触发切换: 表头 + 前 2 行先写出
	tb.AddRow(C("4444"), C("w"))
	tb.Render() // 已切换后无动作, 不重复输出

	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines (tsv header + 4 rows), got %d:\n%q", len(lines), out)
	}
	if lines[0] != "A\tB" {
		t.Errorf("tsv header wrong:\n%q", out)
	}
	if lines[1] != "1\tx" || lines[2] != "22\ty" || lines[3] != "333\tz" || lines[4] != "4444\tw" {
		t.Errorf("plain rows wrong:\n%q", out)
	}
	if strings.Contains(out, "-") {
		t.Errorf("plain mode should have no separator line:\n%q", out)
	}
}

func TestTablePlainCellGate(t *testing.T) {
	// 单单元格超宽时即使行数很少也降级为 TSV
	tb := NewTable("A").PlainRowLimit(1000)
	var buf bytes.Buffer
	tb.To(&buf)

	tb.AddRow(C("short"))
	tb.AddRow(C(strings.Repeat("x", plainCellLimit+1)))
	tb.Render()

	out := buf.String()
	if !strings.HasPrefix(out, "A\nshort\n") {
		t.Fatalf("cell gate should degrade to tsv:\n%q", out)
	}
	if strings.Contains(out, "  ") {
		t.Errorf("plain mode should not pad:\n%q", out)
	}
}

func TestSanitizeTSV(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"a\tb", `a\tb`},
		{"a\nb", `a\nb`},
		{"a\rb", `a\rb`},
	}
	for _, c := range cases {
		if got := sanitizeTSV(c.in); got != c.want {
			t.Errorf("sanitizeTSV(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTableUnderPlainRowLimitStaysAligned(t *testing.T) {
	tb := NewTable("A", "B").PlainRowLimit(10)
	tb.AddRow(C("1"), C("x"))
	tb.AddRow(C("22"), C("y"))
	out := renderTable(t, tb)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("under limit should still render aligned table (header/sep/2 rows), got %d:\n%s", len(lines), out)
	}
}

func TestRuneWidth(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"abc", 3},
		{"中文", 4},
		{"a中b", 4},
		{"héllo", 5},
		{"\x1b[31mRED\x1b[0m", 3}, // ANSI 不计宽
		{"é", 1},                  // e + U+0301 组合
		{"ｱｲｳ", 3},                // 半角假名 = 1
		{"ＡＢＣ", 6},                // 全角字母 = 2
		{"🟢", 2},                  // 0x1F7E2 彩色圆
		{"שָׁלוֹם", 4},            // 希伯来语元音点 (Mn) 归零
	}
	for _, c := range cases {
		if got := displayWidth(c.s); got != c.want {
			t.Errorf("displayWidth(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}
