//go:build !windows

package main

import (
	"context"
	"fmt"
)

// 这份文件让项目在非 Windows 平台（macOS / Linux）上也能编译，
// 方便本地开发与测试。真实的 ACPI / 传感器采集只在 Windows 上可用。

type acpiSource struct{}

func (s *acpiSource) name() string { return "acpi-wmi" }

func (s *acpiSource) collect(context.Context) ([]Reading, error) {
	return nil, fmt.Errorf("仅支持 Windows")
}

type nvidiaSource struct {
	exePath string
}

func (s *nvidiaSource) name() string { return "nvidia-smi" }

func (s *nvidiaSource) collect(context.Context) ([]Reading, error) {
	return nil, fmt.Errorf("仅支持 Windows")
}

type lhmLauncher struct {
	exePath string
	logf    func(string, ...any)
}

func (l *lhmLauncher) ensure(ctx context.Context, url string) {}

func exeDir(path string) string { return "" }
