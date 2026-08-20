//go:build windows

package i18n

import "golang.org/x/sys/windows"

// consoleUTF8 在 Windows 下通过控制台输出代码页判断是否支持中文编码（65001 = UTF-8）。
func consoleUTF8() bool {
	cp, err := windows.GetConsoleOutputCP()
	return err == nil && cp == 65001
}
