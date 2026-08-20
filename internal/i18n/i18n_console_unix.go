//go:build !windows

package i18n

// consoleUTF8 在非 Windows 平台依赖 locale 环境变量判断，
// 未设置任何 locale 变量时按不支持中文编码处理。
func consoleUTF8() bool { return false }
