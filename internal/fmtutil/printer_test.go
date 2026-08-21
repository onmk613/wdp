package fmtutil

import (
	"bytes"
	"strings"
	"testing"
)

func TestColorizeNoColorPassthrough(t *testing.T) {
	s := "hello"
	if got := colorize(s, None, true); got != s {
		t.Errorf("no-color should pass through: %q", got)
	}
	if got := colorize(s, Red, true); got != s {
		t.Errorf("no-color should pass through: %q", got)
	}
}

func TestColorizeAddsEscapeCodes(t *testing.T) {
	got := colorize("hi", Green, false)
	if !strings.Contains(got, "\033[32m") {
		t.Errorf("missing green code: %q", got)
	}
	if !strings.HasSuffix(got, resetCode) {
		t.Errorf("missing reset: %q", got)
	}
}

func TestColorizePreservesTrailingNewline(t *testing.T) {
	// 末尾换行应留在 reset 之后, 不染色
	got := colorize("x\n", Yellow, false)
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("newline lost: %q", got)
	}
	if !strings.Contains(got, "\033[33m") {
		t.Errorf("missing yellow code: %q", got)
	}
}

func TestColorizePureNewlineNotColored(t *testing.T) {
	if got := colorize("\n", Blue, false); got != "\n" {
		t.Errorf("pure newline should not be colored: %q", got)
	}
}

func TestPrinterSetColor(t *testing.T) {
	p := New()
	var buf bytes.Buffer
	p.SetWriter(&buf)

	p.SetColor(false) // 强制关色
	p.Printf(Green, "test")
	if buf.String() != "test" {
		t.Errorf("no-color printf: %q", buf.String())
	}

	buf.Reset()
	p.SetColor(true) // 强制开色
	p.Print(Red, "x")
	if !strings.Contains(buf.String(), "\033[31m") {
		t.Errorf("color printf missing code: %q", buf.String())
	}
}

func TestPrinterColorAutoNonTerminal(t *testing.T) {
	p := New()
	var buf bytes.Buffer // 非 *os.File -> 非终端
	p.SetWriter(&buf)
	p.SetColorAuto()
	p.Printf(Green, "n")
	if buf.String() != "n" {
		t.Errorf("non-terminal auto should be no-color: %q", buf.String())
	}
}

func TestPrinterColorEnabled(t *testing.T) {
	p := New()
	var buf bytes.Buffer
	p.SetWriter(&buf)

	p.SetColor(false)
	if p.ColorEnabled() {
		t.Error("SetColor(false) should disable color")
	}

	p.SetColor(true)
	if !p.ColorEnabled() {
		t.Error("SetColor(true) should enable color")
	}

	p.SetColorAuto() // auto + 非终端 -> 自动关色
	if p.ColorEnabled() {
		t.Error("auto + non-terminal should disable color")
	}
}

func TestNoColorEnvConvention(t *testing.T) {
	// no-color.org 约定: 变量存在且非空即禁色, 与取值无关
	t.Setenv("NO_COLOR", "")
	if noColorByEnv() {
		t.Error("空 NO_COLOR 不应禁色")
	}
	t.Setenv("NO_COLOR", "0")
	if !noColorByEnv() {
		t.Error("NO_COLOR 非空即禁色（取值 0 也是）")
	}

	// 显式强制开色不受 NO_COLOR 影响
	p := New()
	var buf bytes.Buffer
	p.SetWriter(&buf)
	p.SetColor(true)
	if !p.ColorEnabled() {
		t.Error("SetColor(true) 应覆盖 NO_COLOR")
	}
}
