package main

import (
	"os/exec"
	"runtime"
)

// openBrowser 用系统默认浏览器打开一个网址。
//
// Windows 上通过 rundll32 调用 URL 协议处理器，这样即使当前是远程桌面会话
// 也能把页面弹到本机浏览器里。
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
