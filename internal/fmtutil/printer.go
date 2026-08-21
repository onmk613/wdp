package fmtutil

import (
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/term"
)

// colorMode 决定 Printer 的着色策略。
type colorMode int

const (
	colorAuto   colorMode = iota // 自动：按 writer 是否终端
	colorAlways                  // 强制开
	colorNever                   // 强制关
)

// std 是包级默认 Printer（os.Stdout、自动着色），NewTable 未显式绑定时使用。
var std = New()

// Printer 是线程安全的着色输出器：颜色开关与输出目标可运行期调整，
// 表格（NewTable）与直接输出共用同一实例，保证颜色决策一致。
type Printer struct {
	mu            sync.Mutex
	out           io.Writer
	mode          colorMode
	outIsTerminal bool
}

// New 创建写入 os.Stdout、按终端自动着色的 Printer。
func New() *Printer {
	out := os.Stdout
	return &Printer{
		out:           out,
		mode:          colorAuto,
		outIsTerminal: isTerminal(out),
	}
}

// SetColor 强制开/关颜色。
func (p *Printer) SetColor(b bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b {
		p.mode = colorAlways
	} else {
		p.mode = colorNever
	}
}

// SetColorAuto 恢复自动模式（writer 为终端时着色）。
func (p *Printer) SetColorAuto() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = colorAuto
}

// SetWriter 切换输出目标并重测终端属性。
func (p *Printer) SetWriter(w io.Writer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.out = w
	p.outIsTerminal = isTerminal(w)
}

// ColorEnabled 返回当前是否启用颜色输出。
func (p *Printer) ColorEnabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.noColorLocked()
}

// Printf 按指定颜色格式化输出。
func (p *Printer) Printf(c Color, format string, args ...any) {
	p.output(c, fmt.Sprintf(format, args...))
}

// Print 按指定颜色输出。
func (p *Printer) Print(c Color, args ...any) {
	p.output(c, fmt.Sprint(args...))
}

// Println 按指定颜色输出并换行。
func (p *Printer) Println(c Color, args ...any) {
	p.output(c, fmt.Sprintln(args...))
}

// Sprint 返回按当前颜色开关着色后的字符串（不输出），供需要预先
// 拼接多色文本的调用方（如 diff 块、汇总行）使用。
func (p *Printer) Sprint(c Color, s string) string {
	p.mu.Lock()
	noColor := p.noColorLocked()
	p.mu.Unlock()
	return colorize(s, c, noColor)
}

func (p *Printer) output(c Color, s string) {
	p.mu.Lock()
	noColor := p.noColorLocked()
	w := p.out
	p.mu.Unlock()
	_, _ = fmt.Fprint(w, colorize(s, c, noColor))
}

func (p *Printer) noColorLocked() bool {
	switch p.mode {
	case colorAlways:
		return false
	case colorNever:
		return true
	default:
		return !p.outIsTerminal || noColorByEnv()
	}
}

// noColorByEnv 报告 NO_COLOR 约定是否生效：变量存在且非空即禁色
// （无论取值，见 https://no-color.org）。仅在自动模式下参与判定，
// 显式 SetColor(true) 的强制开色不受影响。
func noColorByEnv() bool { return os.Getenv("NO_COLOR") != "" }

// colorize 包裹颜色码；末尾换行符留在 reset 之后，纯换行不着色。
func colorize(s string, c Color, noColor bool) string {
	code, ok := colorCodes[c]
	if noColor || c == None || !ok {
		return s
	}

	end := len(s)
	for end > 0 {
		if b := s[end-1]; b == '\n' || b == '\r' {
			end--
		} else {
			break
		}
	}
	if end == 0 {
		return s
	}

	return code + s[:end] + resetCode + s[end:]
}

// IsTerminal 报告 w 是否连接到终端。基于 term 的 ioctl 探测,
// 比 ModeCharDevice 位判定更准确（/dev/null 不是终端）。
func IsTerminal(w io.Writer) bool { return isTerminal(w) }

// ColorAuto 返回当前环境的缺省颜色决策：w 为终端且 NO_COLOR 未生效。
// CLI 的 --no-color 等显式开关应调用 SetColor(false) 覆盖本判定。
func ColorAuto(w io.Writer) bool { return isTerminal(w) && !noColorByEnv() }

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
