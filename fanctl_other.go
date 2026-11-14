//go:build !windows

package main

import "errors"

// unsupportedBackend 是非 Windows 平台上的占位后端。
// 风扇调速依赖 Windows 的 PawnIO 内核驱动，其他平台一律明确告知不支持。
type unsupportedBackend struct{}

func (unsupportedBackend) Info() FanBackendInfo {
	return FanBackendInfo{
		Available: false,
		Kind:      "none",
		Detail:    "风扇调速依赖 Windows 的 PawnIO 内核驱动，当前系统不支持",
	}
}

func (unsupportedBackend) SetManual(int, float64) error {
	return errors.New("风扇调速仅在 Windows 上可用")
}

func (unsupportedBackend) Restore(int) error { return nil }

func (unsupportedBackend) Duty(int) (float64, bool) { return 0, false }

// newPlatformBackend 在非 Windows 平台上返回不可用后端，且没有硬件数据源。
func newPlatformBackend(bool) (fanBackend, []source) {
	return unsupportedBackend{}, nil
}
