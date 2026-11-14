//go:build !windows

package main

import "errors"

// runAsService 在非 Windows 平台不可用。
func runAsService(start func() (func() error, error)) error {
	return errors.New("服务模式仅支持 Windows")
}
