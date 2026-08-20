package fmtutil

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
)

// Cell 表格单元格；Color 为 None 时使用默认颜色。
// Text 应为纯文本（不含 ANSI 转义），着色请使用 Color 字段。
type Cell struct {
	Text  string
	Color Color
}

// C 构造普通单元格。
func C(text string) Cell { return Cell{Text: text} }

// CC 构造带色单元格。
func CC(text string, c Color) Cell { return Cell{Text: text, Color: c} }

// CCf 构造带色格式化单元格。
func CCf(c Color, format string, a ...any) Cell {
	return Cell{Text: fmt.Sprintf(format, a...), Color: c}
}

// Cf 构造格式化单元格。
func Cf(format string, a ...any) Cell { return Cell{Text: fmt.Sprintf(format, a...)} }

// Table 轻量文本表格渲染器：
//   - 表头 + 分隔线 + 数据行，按内容自动计算列宽
//   - 支持 CJK 全角/宽字符宽度计算，中文表头对齐不偏移
//   - 单元格可单独着色，颜色开关遵循所绑定的 Printer（--no-color / 非终端自动禁用）
//   - 不做边框、不换行、不合并单元格
type Table struct {
	header    []string
	align     []bool // true = 右对齐（适合数字列）
	rows      [][]Cell
	added     int                 // 已添加的行数 (plain 模式下同样累计)
	bufBytes  int                 // 缓冲文本字节数 (降级闸门用)
	limit     int                 // >0: 行数上限, 超过后转为流式 TSV 输出
	plain     bool                // 已切换为流式输出, 不再缓存行
	sink      func(Color, string) // nil: 全局 std; 否则输出到自定义目标
	plainSink bool                // sink 为纯文本目标 (To), 渲染时不着色
}

// NewTable 新建绑定全局标准输出的表格（颜色遵循全局 std 开关）；
// headers 为表头，传空表示无表头行。
func NewTable(headers ...string) *Table {
	return &Table{header: headers, align: make([]bool, len(headers))}
}

// NewTable 新建绑定 p 的表格：单元格着色与否由 p 的颜色开关决定。
func (p *Printer) NewTable(headers ...string) *Table {
	t := NewTable(headers...)
	t.sink = p.output
	return t
}

// To 指定纯文本输出目标: 所有输出 (含 plain 流式行) 以无 ANSI 形态写入 w。
// 须在第一次 AddRow 之前调用, 否则 plain 模式下已流出的行不会进入 w.
func (t *Table) To(w io.Writer) *Table {
	t.sink = func(_ Color, s string) { fmt.Fprint(w, s) }
	t.plainSink = true
	return t
}

// AlignRight 把指定列设为右对齐（适合数字列）。
func (t *Table) AlignRight(cols ...int) *Table {
	for _, c := range cols {
		if c < 0 {
			continue
		}
		for len(t.align) <= c {
			t.align = append(t.align, false)
		}
		t.align[c] = true
	}
	return t
}

// PlainRowLimit 设置降级闸门: 行数、单单元格显示宽度、累计缓冲字节
// 任一超过上限后, 不再缓存行做对齐, 改为 TSV 风格逐行流式输出
// (制表符分隔, 首行输出表头, 字段内 \t\n\r 转义; 可配合
// `column -t -s$'\t'` 还原对齐, `cut -f` 正确取列),
// 使超大输出 (如数千主机 RECAP) 的内存与首行延迟有界. 0 (默认) 表示始终对齐。
func (t *Table) PlainRowLimit(n int) *Table {
	t.limit = n
	return t
}

// Len 返回已添加的行数 (切换为 plain 流式后同样累计).
func (t *Table) Len() int { return t.added }

// AddRow 追加一行数据. 触发降级闸门后, 先以 TSV 风格输出表头与已缓存
// 行, 随后每行到达即直接写出, 不再占用内存.
func (t *Table) AddRow(cells ...Cell) *Table {
	t.added++
	if t.plain || t.overLimit(cells) {
		if !t.plain {
			t.plain = true
			if len(t.header) > 0 {
				hc := make([]Cell, len(t.header))
				for i, h := range t.header {
					hc[i] = Cell{Text: h}
				}
				t.emitPlainRow(hc)
			}
			for _, r := range t.rows {
				t.emitPlainRow(r)
			}
			t.rows = nil
		}
		t.emitPlainRow(cells)
		return t
	}
	t.rows = append(t.rows, cells)
	return t
}

// overLimit 判断是否触发降级闸门 (行数 / 单单元格显示宽度 / 缓冲字节).
// 仅在启用 PlainRowLimit 时生效; 顺带累计缓冲字节数.
func (t *Table) overLimit(cells []Cell) bool {
	if t.limit == 0 {
		return false
	}
	if len(t.rows) >= t.limit {
		return true
	}
	for _, c := range cells {
		t.bufBytes += len(c.Text) + 8
		if displayWidth(c.Text) > plainCellLimit {
			return true
		}
	}
	return t.bufBytes >= plainByteLimit
}

// Render 渲染输出（颜色遵循所绑定 Printer 的开关；To(w) 时为纯文本）。
// 已切换为流式输出时无动作 (行已逐条写出).
func (t *Table) Render() {
	if t.plain {
		return
	}
	t.render(t.emit, !t.plainSink)
}

// RenderTo 以纯文本（无 ANSI 颜色）渲染到 w，便于测试断言与文件输出。
func (t *Table) RenderTo(w io.Writer) {
	t.To(w)
	if t.plain {
		return
	}
	t.render(t.emit, false)
}

// emit 按 sink 输出: sink 非 nil 时写入自定义目标; 否则走全局 std.
func (t *Table) emit(c Color, s string) {
	if t.sink != nil {
		t.sink(c, s)
		return
	}
	std.output(c, s)
}

// emitPlainRow 以 TSV 风格输出一行: 单元格文本以制表符分隔 (字段内
// \t\n\r 转义), 逐格着色; 无对齐无边框, 用于降级后的流式输出.
func (t *Table) emitPlainRow(cells []Cell) {
	for i, c := range cells {
		if i > 0 {
			t.emit(None, "\t")
		}
		t.emit(c.Color, sanitizeTSV(c.Text))
	}
	t.emit(None, "\n")
}

// tsvReplacer 转义字段内的制表符与换行, 避免下游按列切分时错位.
var tsvReplacer = strings.NewReplacer("\t", `\t`, "\n", `\n`, "\r", `\r`)

func sanitizeTSV(s string) string {
	if !strings.ContainsAny(s, "\t\n\r") {
		return s
	}
	return tsvReplacer.Replace(s)
}

// plainCellLimit / plainByteLimit 启用 PlainRowLimit 时的降级闸门
// (行数之外的补充): 单单元格显示宽度或累计缓冲字节超过后同样转为
// TSV 流式, 防止少量超宽单元格 (超长 key、大段错误信息) 撑爆表格对齐.
const (
	plainCellLimit = 200
	plainByteLimit = 4 << 20
)

// render 计算列宽并逐行输出。color 为 false 时全部按纯文本输出。
func (t *Table) render(emit func(Color, string), color bool) {
	if len(t.rows) == 0 {
		return
	}

	// 列数取表头与所有行的最大值, 避免行超长时静默丢列
	ncol := len(t.header)
	for _, row := range t.rows {
		ncol = max(ncol, len(row))
	}
	if ncol == 0 {
		return
	}

	// 列对齐标志: 拷贝避免修改 t.align (无表头时也不会越界)
	align := make([]bool, ncol)
	copy(align, t.align)

	width := make([]int, ncol)
	for i, h := range t.header {
		width[i] = max(width[i], displayWidth(h))
	}
	for _, row := range t.rows {
		for i, c := range row {
			width[i] = max(width[i], displayWidth(c.Text))
		}
	}

	var sb strings.Builder
	emitLine := func(cells []Cell, header bool) {
		// 整行无着色需求（颜色关闭 / 所有单元格无色）时拼接成一行一次写出,
		// 减少输出调用次数; 表头行在开色时始终加粗蓝色, 走逐格着色路径
		plain := !color
		if color {
			plain = true
			for _, c := range cells {
				if c.Color != None {
					plain = false
					break
				}
			}
			if header {
				plain = false
			}
		}
		if plain {
			sb.Reset()
			for i := 0; i < ncol; i++ {
				text := ""
				if i < len(cells) {
					text = cells[i].Text
				}
				sb.WriteString(padCell(text, width[i], align[i], i == ncol-1))
				if i < ncol-1 {
					sb.WriteString("  ")
				}
			}
			sb.WriteString("\n")
			emit(None, sb.String())
			return
		}
		for i := 0; i < ncol; i++ {
			text, clr := "", None
			if i < len(cells) {
				text, clr = cells[i].Text, cells[i].Color
			}
			if header {
				clr = BoldBlue
			}
			emit(clr, padCell(text, width[i], align[i], i == ncol-1))
			if i < ncol-1 {
				emit(None, "  ")
			}
		}
		emit(None, "\n")
	}

	if len(t.header) > 0 {
		hcells := make([]Cell, len(t.header))
		for i, h := range t.header {
			hcells[i] = Cell{Text: h}
		}
		emitLine(hcells, true)
		seps := make([]string, ncol)
		for i := range seps {
			seps[i] = strings.Repeat("-", width[i])
		}
		emit(Dim, strings.TrimRight(strings.Join(seps, "  "), " "))
		emit(None, "\n")
	}

	for _, row := range t.rows {
		emitLine(row, false)
	}
}

// padCell 按列宽与对齐方式填充单元格文本。
// 右对齐补前导空格（末列也生效）；左对齐末列不补尾空格。
func padCell(text string, w int, right, last bool) string {
	pad := w - displayWidth(text)
	if pad <= 0 {
		return text
	}
	if right {
		return strings.Repeat(" ", pad) + text
	}
	if last {
		return text
	}
	return text + strings.Repeat(" ", pad)
}

// ansiRe 匹配 ANSI 转义序列（CSI），用于宽度计算前剥离着色码。
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// displayWidth 计算字符串的终端显示宽度（CJK 宽字符按 2 计，
// 组合符号按 0 计，混入的 ANSI 颜色码不占宽度）。
func displayWidth(s string) int {
	if strings.IndexByte(s, 0x1b) >= 0 {
		s = ansiRe.ReplaceAllString(s, "")
	}
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// DisplayWidth 计算字符串的终端显示宽度，语义与内部 displayWidth 相同，
// 供包外复用（如截断长消息时按显示宽度计算）。
func DisplayWidth(s string) int { return displayWidth(s) }

// TruncateDisplay 按终端显示宽度截断字符串（附加省略号 …），
// 用于表格单元格等定宽场景; s 已短于 max 时原样返回。
func TruncateDisplay(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if displayWidth(s) <= max {
		return s
	}
	var sb strings.Builder
	w := 0
	for _, r := range s {
		rw := runeWidth(r)
		if w+rw > max-1 {
			break
		}
		sb.WriteRune(r)
		w += rw
	}
	return sb.String() + "…"
}

// runeWidth 返回单个 rune 的终端显示宽度。
// 依据 Unicode East Asian Width 手工整理, 组合符号经 unicode.Mn/Me 归零,
// 覆盖 CJK 与常见 emoji. 多 rune 合成一个显示格的场景 (emoji ZWJ 家庭、
// 区域指示符国旗、keycap 序列) 需 grapheme 聚合才精确,
// 如需 100% 正确可换用 rivo/uniseg.
func runeWidth(r rune) int {
	switch {
	// 控制符 (C0/C1 + DEL)
	case r < 0x20 || r == 0x7F || in(r, 0x80, 0x9F):
		return 0
	// 零宽格式符: ZWNJ/ZWJ、零宽空格、双向标记、词连接符、BOM
	case r == 0x200C || r == 0x200D ||
		r == 0x200B || in(r, 0x200E, 0x200F) ||
		in(r, 0x2060, 0x2064) || r == 0xFEFF:
		return 0
	// 组合附加符号（零宽）: 变体选择符、假名浊点、泰语/希伯来语/天城文等
	case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
		return 0
	// CJK 全角/宽字符
	case in(r, 0x1100, 0x115F) || // 谚文字母 (Jamo)
		r == 0x2329 || r == 0x232A ||
		in(r, 0x2E80, 0x303E) || // CJK 部首补充/康熙部首/CJK 符号标点/平假名/片假名
		in(r, 0x3041, 0x33FF) || // 假名、CJK 兼容、方框绘制、CJK 笔画
		in(r, 0x3400, 0x4DBF) || // CJK 扩展 A
		in(r, 0x4E00, 0x9FFF) || // CJK 统一表意
		in(r, 0xA000, 0xA4CF) || // 彝文
		in(r, 0xA960, 0xA97F) || // 谚文字母扩展 A
		in(r, 0xAC00, 0xD7A3) || // 谚文音节
		in(r, 0xD7B0, 0xD7FF) || // 谚文字母扩展 B
		in(r, 0xF900, 0xFAFF) || // CJK 兼容表意
		in(r, 0xFE10, 0xFE19) ||
		in(r, 0xFE30, 0xFE6F) ||
		in(r, 0xFF00, 0xFF60) ||
		in(r, 0xFFE0, 0xFFE6) ||
		in(r, 0x16FE0, 0x18AFF) || // 西夏文/女书等
		in(r, 0x1B000, 0x1B16F) || // 假名补充/假名扩展 A
		in(r, 0x1F200, 0x1F2FF) || // 封闭表意补充
		in(r, 0x20000, 0x2FFFD) ||
		in(r, 0x30000, 0x3FFFD):
		return 2
	// 常见 emoji（终端普遍渲染为 2 列）
	case r == 0x231A || r == 0x231B || // 手表/沙漏
		in(r, 0x23E9, 0x23EC) || r == 0x23F0 || r == 0x23F3 ||
		r == 0x25FD || r == 0x25FE ||
		r == 0x2614 || r == 0x2615 ||
		r == 0x2648 || r == 0x2649 || r == 0x264A || r == 0x264B ||
		r == 0x264C || r == 0x264D || r == 0x264E || r == 0x264F ||
		r == 0x2650 || r == 0x2651 || r == 0x2652 || r == 0x2653 ||
		r == 0x26AA || r == 0x26AB || r == 0x26BD || r == 0x26BE ||
		r == 0x26C4 || r == 0x26C5 || r == 0x26CE || r == 0x26D4 ||
		r == 0x26EA || r == 0x26F2 || r == 0x26F3 || r == 0x26F5 ||
		r == 0x26FA || r == 0x26FD ||
		r == 0x2705 || r == 0x270A || r == 0x270B || r == 0x2728 ||
		r == 0x274C || r == 0x274E ||
		in(r, 0x2753, 0x2755) || r == 0x2757 ||
		in(r, 0x2795, 0x2797) || r == 0x27B0 || r == 0x27BF ||
		r == 0x2B1B || r == 0x2B1C || r == 0x2B50 || r == 0x2B55 ||
		in(r, 0x1F000, 0x1F0FF) || // 麻将牌/扑克牌
		in(r, 0x1F1E6, 0x1F1FF) || // 区域指示符（国旗）
		in(r, 0x1F300, 0x1F64F) ||
		in(r, 0x1F680, 0x1F6FF) ||
		in(r, 0x1F7E0, 0x1F7EB) || // 彩色圆/方 (🟢🔴🟡)
		in(r, 0x1F900, 0x1F9FF) ||
		in(r, 0x1FA70, 0x1FAFF):
		return 2
	default:
		return 1
	}
}

// in 判断 rune 是否位于 [lo, hi] 闭区间.
func in(r, lo, hi rune) bool {
	return r >= lo && r <= hi
}
