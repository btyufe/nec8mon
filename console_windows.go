//go:build windows

package main

import (
	"io"
	"os"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// enableUTF8Console 把控制台切换到 UTF-8 代码页。
//
// 简体中文 Windows 的默认控制台代码页是 936(GBK)，程序输出 UTF-8 字节会被
// 显示成乱码（现场诊断报告里最典型的表现）。切到 65001(UTF-8) 之后，
// 大部分场景已经能正常显示。
func enableUTF8Console() {
	_ = windows.SetConsoleOutputCP(65001)
	_ = windows.SetConsoleCP(65001)
}

// isConsoleHandle 判断句柄是不是一个真正的控制台。
//
// 输出被重定向到文件或管道时不是控制台，这时应该直接写 UTF-8 字节。
func isConsoleHandle(h windows.Handle) bool {
	var mode uint32
	return windows.GetConsoleMode(h, &mode) == nil
}

// consoleWrite 把文本写到控制台。
//
// 对真正的控制台句柄改用 WriteConsoleW（Unicode 通道），
// 这样彻底绕开代码页，简体中文在任何 Windows 上都不会乱码；
// 非控制台（重定向到文件/管道）则直接写 UTF-8 字节。
func consoleWrite(f *os.File, s string) {
	if s == "" {
		return
	}
	h := windows.Handle(f.Fd())
	if !isConsoleHandle(h) {
		_, _ = f.WriteString(s)
		return
	}
	buf := utf16.Encode([]rune(s))
	if len(buf) == 0 {
		return
	}
	var written uint32
	if err := windows.WriteConsole(h, &buf[0], uint32(len(buf)), &written, nil); err != nil {
		// 极少数情况下 WriteConsoleW 会失败（例如刚启动还没初始化好），
		// 退回普通写入，至少不丢内容。
		_, _ = f.WriteString(s)
	}
}

// consoleWriter 让 log 包的输出也走 WriteConsoleW，
// 避免服务/控制台模式下的中文日志显示成乱码。
type consoleWriter struct {
	w *os.File
}

func (c consoleWriter) Write(p []byte) (int, error) {
	consoleWrite(c.w, string(p))
	return len(p), nil
}

// newConsoleWriter 返回给 log.SetOutput 用的写入器。
func newConsoleWriter() io.Writer {
	return consoleWriter{w: os.Stderr}
}
