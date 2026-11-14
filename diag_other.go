//go:build !windows

package main

import "fmt"

// runDiag 在非 Windows 平台上只说明诊断不可用。
func runDiag(string) int {
	fmt.Println("诊断功能仅在 Windows 上可用")
	return 1
}
