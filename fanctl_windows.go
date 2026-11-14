//go:build windows

package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"sync"
)

// lpcIOModule 是 PawnIO 的 LPC/SuperIO 模块，内嵌进可执行文件，
// 这样部署时不需要额外拷贝 DLL 或驱动相关文件。
//
// 该模块来自 LibreHardwareMonitor v0.9.6（Mozilla Public License 2.0），
// 它实现了我们需要的端口 I/O 与 SuperIO 寄存器访问原语。
//
//go:embed assets/LpcIO.bin
var lpcIOModule []byte

// intelMSRModule 是 PawnIO 的 Intel MSR 模块，用于读取 Intel CPU 的
// 封装温度（IA32_THERM_STATUS 等）。
//
// 同样来自 LibreHardwareMonitor v0.9.6（Mozilla Public License 2.0）。
//
//go:embed assets/IntelMSR.bin
var intelMSRModule []byte

// hwBackend 持有 PawnIO 句柄与识别出的 Super I/O 芯片。
//
// 数据源（读温度、转速）与调速后端（写占空比）共用同一个实例，
// 避免重复探测芯片，也保证访问芯片时共用同一把 ISA 总线锁。
//
// PawnIO 的模块是按设备句柄加载的，因此 SuperIO 与 MSR 各用一个句柄，
// 与 LibreHardwareMonitor 的做法一致。
type hwBackend struct {
	mu      sync.Mutex
	pawnLpc *PawnIO
	pawnMSR *PawnIO
	sio     *SuperIO
	detail  string
}

// newHWBackend 打开 PawnIO、加载模块并探测 Super I/O 芯片。
//
// enabled 为假时直接返回一个不可用后端（用于完全禁用硬件访问的场景）。
func newHWBackend(enabled bool) *hwBackend {
	b := &hwBackend{}
	if !enabled {
		b.detail = "已按参数禁用硬件访问"
		return b
	}

	pawn, err := OpenPawnIO()
	if err != nil {
		b.detail = err.Error()
		return b
	}
	b.pawnLpc = pawn

	if err := pawn.LoadModule(lpcIOModule); err != nil {
		b.detail = err.Error()
		return b
	}

	sio, err := DetectSuperIO(pawn)
	if sio != nil {
		b.sio = sio
	}
	switch {
	case err != nil && sio == nil:
		b.detail = fmt.Sprintf("Super I/O 探测失败: %v", err)
	case err != nil:
		// 芯片识别成功但寄存器表未实现：仍可作为只读数据源。
		b.detail = err.Error()
	}
	if b.sio != nil {
		log.Printf("[hw] 已识别 Super I/O: %s（基址 0x%04X，%d 个风扇通道）",
			b.sio.ChipName(), b.sio.BasePort(), b.sio.ChannelCount())
	}
	return b
}

// SuperIO 返回识别出的芯片，没有则返回 nil。
func (b *hwBackend) SuperIO() *SuperIO {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sio
}

// Detail 返回后端状态说明。
func (b *hwBackend) Detail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.detail
}

// MSR 打开（或复用）Intel MSR 用的 PawnIO 句柄。失败时返回 nil。
func (b *hwBackend) MSR() *PawnIO {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pawnMSR != nil {
		return b.pawnMSR
	}
	pawn, err := OpenPawnIO()
	if err != nil {
		return nil
	}
	if err := pawn.LoadModule(intelMSRModule); err != nil {
		pawn.Close()
		return nil
	}
	b.pawnMSR = pawn
	return pawn
}

// Close 释放 PawnIO 句柄。
func (b *hwBackend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pawnLpc != nil {
		b.pawnLpc.Close()
		b.pawnLpc = nil
	}
	if b.pawnMSR != nil {
		b.pawnMSR.Close()
		b.pawnMSR = nil
	}
	b.sio = nil
}

// newPlatformBackend 是平台相关的初始化入口。
//
// 返回调速后端与额外的硬件数据源。非 Windows 平台返回不可用后端。
// fanControl 为假时仍然可以读取传感器，只是禁止写风扇寄存器。
func newPlatformBackend(fanControl bool) (fanBackend, []source) {
	b := newHWBackend(true)

	var sources []source
	if b.SuperIO() != nil {
		sources = append(sources, &superIOSource{backend: b})
	}
	sources = append(sources, &msrCPUSource{backend: b})
	return &superIOBackend{backend: b, control: fanControl}, sources
}

// ------------------------------------------------------------------
// 直接读取 Super I/O 的传感器数据源
// ------------------------------------------------------------------

// superIOSource 直接从 Super I/O 芯片读取温度与风扇转速。
//
// 这是本程序的主数据源：不依赖 LibreHardwareMonitor 常驻，
// 只需要 PawnIO 驱动，结果稳定、开销小。
type superIOSource struct {
	backend *hwBackend
}

func (s *superIOSource) name() string { return "superio" }

func (s *superIOSource) collect(_ context.Context) ([]Reading, error) {
	sio := s.backend.SuperIO()
	if sio == nil {
		detail := s.backend.Detail()
		if detail == "" {
			detail = "未检测到 Super I/O 芯片"
		}
		return nil, fmt.Errorf("%s", detail)
	}

	var out []Reading

	temps, err := sio.Temperatures()
	if err != nil {
		return nil, fmt.Errorf("读取温度失败: %w", err)
	}
	for _, t := range temps {
		out = append(out, Reading{
			ID:       fmt.Sprintf("superio/temp/%s", t.Name),
			Name:     t.Name,
			Category: catTemperature,
			Hardware: sio.ChipName(),
			Value:    t.Value,
			Unit:     "°C",
			Source:   "superio",
		})
	}

	fans, err := sio.Fans()
	if err != nil {
		return nil, fmt.Errorf("读取风扇转速失败: %w", err)
	}
	for _, f := range fans {
		// 可调速通道用 superio/fan/<通道号>，只读测速口用 superio/tach/<下标>。
		// 两者分开，界面与调速逻辑才能准确区分"能调的"和"只能看的"。
		id := fmt.Sprintf("superio/fan/%d", f.Channel)
		if f.Channel < 0 {
			id = fmt.Sprintf("superio/tach/%d", f.Index)
		}
		out = append(out, Reading{
			ID:       id,
			Name:     f.Name,
			Category: catFan,
			Hardware: sio.ChipName(),
			Value:    f.RPM,
			Unit:     "RPM",
			Source:   "superio",
		})
		if f.HasDuty {
			out = append(out, Reading{
				ID:       fmt.Sprintf("superio/control/%d", f.Channel),
				Name:     f.Name + " 占空比",
				Category: "Control",
				Hardware: sio.ChipName(),
				Value:    f.Duty,
				Unit:     "%",
				Source:   "superio",
			})
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("没有可用读数")
	}
	return out, nil
}

// ------------------------------------------------------------------
// 调速后端
// ------------------------------------------------------------------

// superIOBackend 把 SuperIO 芯片包装成统一的调速后端。
type superIOBackend struct {
	backend *hwBackend
	// control 为假时只读不写，禁止修改风扇寄存器。
	control bool
}

// Info 描述后端状态。
func (b *superIOBackend) Info() FanBackendInfo {
	sio := b.backend.SuperIO()
	if sio == nil {
		detail := b.backend.Detail()
		if detail == "" {
			detail = "未检测到可控风扇的 Super I/O 芯片"
		}
		return FanBackendInfo{Available: false, Kind: "superio", Detail: detail}
	}
	if !sio.SupportsControl() {
		detail := b.backend.Detail()
		if detail == "" {
			detail = "该芯片的寄存器表尚未实现"
		}
		return FanBackendInfo{
			Available: false,
			Kind:      "superio",
			Chip:      sio.ChipName(),
			Detail:    detail,
		}
	}
	if !b.control {
		return FanBackendInfo{
			Available: false,
			Kind:      "superio",
			Chip:      sio.ChipName(),
			Detail:    "风扇调速已通过 -fan=false 关闭（仍可读取温度与转速）",
			Channels:  sio.ChannelCount(),
		}
	}
	return FanBackendInfo{
		Available: true,
		Kind:      "superio",
		Chip:      sio.ChipName(),
		Detail:    fmt.Sprintf("寄存器基址 0x%04X，%d 个可控通道", sio.BasePort(), sio.ChannelCount()),
		Channels:  sio.ChannelCount(),
	}
}

// SetManual 接管风扇通道并写入占空比。
func (b *superIOBackend) SetManual(channel int, percent float64) error {
	if !b.control {
		return fmt.Errorf("风扇调速已通过 -fan=false 关闭")
	}
	sio := b.backend.SuperIO()
	if sio == nil {
		return fmt.Errorf("Super I/O 不可用：%s", b.backend.Detail())
	}
	return sio.SetManual(channel, percent)
}

// Restore 把通道交还硬件自动控制。
func (b *superIOBackend) Restore(channel int) error {
	sio := b.backend.SuperIO()
	if sio == nil {
		return nil
	}
	return sio.Restore(channel)
}

// Duty 读取当前占空比。
func (b *superIOBackend) Duty(channel int) (float64, bool) {
	sio := b.backend.SuperIO()
	if sio == nil {
		return 0, false
	}
	return sio.Duty(channel)
}

// RestoreLeftover 还原上次进程异常退出时残留的手动模式。
func (b *superIOBackend) RestoreLeftover() error {
	sio := b.backend.SuperIO()
	if sio == nil {
		return nil
	}
	return sio.RestoreLeftover()
}

// Close 释放底层 PawnIO 句柄。
func (b *superIOBackend) Close() {
	b.backend.Close()
}
