//go:build windows

package main

import (
	"context"
	"fmt"
)

// Intel 的温度相关 MSR。
const (
	msrThermStatus        = 0x019C // IA32_THERM_STATUS
	msrTemperatureTarget  = 0x01A2 // IA32_TEMPERATURE_TARGET
	msrPackageThermStatus = 0x01B1 // IA32_PACKAGE_THERM_STATUS
)

// msrCPUSource 通过 Intel MSR 读取 CPU 封装温度。
//
// 这是 Super I/O 之外的一条独立温度来源：Super I/O 读到的是主板上
// CPU 附近的探头值，MSR 读到的是 CPU 封装本身的温度，更准确。
//
// 只有 Intel CPU 且 PawnIO 的 IntelMSR 模块可用时才有数据；
// AMD CPU 上这个源会安静地返回空。
type msrCPUSource struct {
	backend *hwBackend
}

func (s *msrCPUSource) name() string { return "intel-msr" }

func (s *msrCPUSource) collect(_ context.Context) ([]Reading, error) {
	pawn := s.backend.MSR()
	if pawn == nil {
		return nil, fmt.Errorf("Intel MSR 模块不可用（需要 Intel CPU 与 PawnIO 驱动）")
	}

	// TJMax：CPU 的最高设计温度，来自 IA32_TEMPERATURE_TARGET 的高 16 位。
	target, err := pawn.ReadMSR(msrTemperatureTarget)
	if err != nil {
		return nil, err
	}
	tjMax := float64((target >> 16) & 0xFF)
	if tjMax <= 0 {
		return nil, fmt.Errorf("读取 TJMax 失败")
	}

	var out []Reading

	// 封装温度：IA32_PACKAGE_THERM_STATUS。
	if status, err := pawn.ReadMSR(msrPackageThermStatus); err == nil {
		if eax := uint32(status); eax&0x80000000 != 0 {
			deltaT := float64((eax & 0x007F0000) >> 16)
			temp := tjMax - deltaT
			if temp > 0 && temp < 125 {
				out = append(out, Reading{
					ID:       "intel-msr/package",
					Name:     "CPU Package",
					Category: catTemperature,
					Hardware: "Intel CPU",
					Value:    temp,
					Unit:     "°C",
					Source:   "intel-msr",
				})
			}
		}
	}

	// 核心温度：IA32_THERM_STATUS（读的是当前线程所在的核）。
	if status, err := pawn.ReadMSR(msrThermStatus); err == nil {
		if eax := uint32(status); eax&0x80000000 != 0 {
			deltaT := float64((eax & 0x007F0000) >> 16)
			temp := tjMax - deltaT
			if temp > 0 && temp < 125 {
				out = append(out, Reading{
					ID:       "intel-msr/core",
					Name:     "Core Max",
					Category: catTemperature,
					Hardware: "Intel CPU",
					Value:    temp,
					Unit:     "°C",
					Source:   "intel-msr",
				})
			}
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("没有读到有效的 CPU 温度")
	}
	return out, nil
}
