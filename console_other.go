//go:build !windows

package main

import (
	"io"
	"os"
)

// enableUTF8Console 在非 Windows 平台上无需处理控制台代码页。
func enableUTF8Console() {}

// consoleWrite 直接写标准输出。
func consoleWrite(f *os.File, s string) {
	_, _ = f.WriteString(s)
}

// consoleWriter 让 log 包的输出直接写标准错误。
type consoleWriter struct {
	w *os.File
}

func (c consoleWriter) Write(p []byte) (int, error) {
	_, _ = c.w.Write(p)
	return len(p), nil
}

// newConsoleWriter 返回给 log.SetOutput 用的写入器。
func newConsoleWriter() io.Writer {
	return consoleWriter{w: os.Stderr}
}
