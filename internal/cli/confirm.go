package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"wdp/internal/chart"
	"wdp/internal/config"
	"wdp/internal/fmtutil"
)

// confirmReversibility 打印 chart 指定相位任务的可逆性评估（着色遵循全局
// 颜色开关，与 buildReporter 同一决策：config 允许 && stderr 为终端且
// NO_COLOR 未设）。按相位评估：uninstall 评估 uninstall.yaml 的删除任务，
// 不再误用 deploy.yaml。
func confirmReversibility(ch *chart.Chart, phase string, yes bool) error {
	rep := ch.Analyze(phase)
	p := fmtutil.New()
	p.SetWriter(os.Stderr)
	p.SetColor(config.Current().Color() && fmtutil.ColorAuto(os.Stderr))

	p.Print(fmtutil.BoldCyan, "==> chart assessment")
	p.Printf(fmtutil.Bold, " [%s %s]\n", ch.Meta.Name, ch.Meta.Version)
	for _, row := range rep.Rows() {
		p.Printf(fmtutil.None, "    %-20s %s", row.Label, p.Sprint(rowColor(row.Label), fmt.Sprintf("%3d", row.Count)))
		if row.Note != "" {
			p.Printf(fmtutil.None, "  %s", p.Sprint(fmtutil.Dim, "("+row.Note+")"))
		}
		p.Print(fmtutil.None, "\n")
	}
	for _, e := range rep.Examples {
		p.Printf(fmtutil.Yellow, "      - %s\n", e)
	}
	lcClr := fmtutil.None
	switch {
	case rep.HasUninstall && rep.AutoRollback:
		lcClr = fmtutil.Green
	case !rep.HasUninstall && !rep.AutoRollback:
		lcClr = fmtutil.Yellow // 既不可卸载也无自动回滚，提示风险
	}
	p.Printf(fmtutil.None, "    %s %s\n", p.Sprint(fmtutil.Dim, "lifecycle:"), p.Sprint(lcClr, rep.LifecycleNote()))

	if rep.Irreversible == 0 || yes {
		return nil
	}
	if !fmtutil.IsTerminal(os.Stderr) { // 与提示输出流一致：stdout 重定向（管道/CI 摘取）不应跳过确认
		// stderr 非终端不再静默放行：交互会话里常见的
		// `wdp run … 2>&1 | tee deploy.log` 也落进这条分支，用户并不
		// 知道自己跳过了不可逆确认。要么 --yes 显式确认，要么用环境
		// 变量保留旧的自动放行（CI 场景）。
		if os.Getenv("WDP_AUTO_CONFIRM_IRREVERSIBLE") == "1" {
			p.Printf(fmtutil.Yellow, "==> warning: %d irreversible operation(s), continuing (non-interactive, WDP_AUTO_CONFIRM_IRREVERSIBLE=1)\n",
				rep.Irreversible)
			return nil
		}
		return fmt.Errorf("irreversible operations detected in a non-interactive environment (stderr is not a terminal); " +
			"rerun with --yes to proceed explicitly, or set WDP_AUTO_CONFIRM_IRREVERSIBLE=1 for unattended pipelines")
	}
	p.Printf(fmtutil.None, "==> %s, continue? [Y/n] ",
		p.Sprint(fmtutil.BoldRed, "irreversible operations detected"))
	line, err := readLine(os.Stdin)
	if err != nil {
		// fail-closed：stdin 被重定向/关闭（如 `< /dev/null`）时 EOF——
		// 不可逆操作不能因读不到确认而默认放行（终端判定在 stderr，
		// 二者可以不一致）；提示用户改用 --yes 显式确认
		return fmt.Errorf("cannot read confirmation (stdin closed or redirected); rerun with --yes to proceed explicitly")
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return nil
	default:
		return fmt.Errorf("deployment cancelled by user (irreversible-operation confirmation declined)")
	}
}

// rowColor 评估分类行的语义色：可逆绿 / 部分可逆黄 / 只读弱化 / 不可逆加粗红。
func rowColor(label string) fmtutil.Color {
	switch label {
	case "reversible":
		return fmtutil.Green
	case "partially reversible":
		return fmtutil.Yellow
	case "read-only":
		return fmtutil.Dim
	default:
		return fmtutil.BoldRed
	}
}

func readLine(r io.Reader) (string, error) {
	buf := make([]byte, 0, 64)
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return string(buf), nil
			}
			if one[0] != '\r' {
				buf = append(buf, one[0])
			}
		}
		if err != nil {
			if len(buf) > 0 {
				return string(buf), nil
			}
			return "", err
		}
	}
}
