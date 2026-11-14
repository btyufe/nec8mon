//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// PawnIO 驱动的设备路径与 IOCTL 定义。
// 这些常量与 LibreHardwareMonitor v0.9.6 的 PawnIo.cs 保持一致，
// 因为我们复用了它同名的 PawnIO 模块（LpcIO.bin / IntelMSR.bin）。
const (
	pawnIODevicePath = `\\?\GLOBALROOT\Device\PawnIO`
	pawnIODeviceType = uint32(41394) << 16
	ioctlPioLoadBin  = uint32(0x821) << 2
	ioctlPioExecFn   = uint32(0x841) << 2

	// 模块函数名在输入缓冲区里占 32 字节，ASCII、后面补 0。
	pawnIOFnNameLen = 32
)

// PawnIO 是对 PawnIO 内核驱动的极简客户端。
//
// PawnIO 提供受控的端口 I/O 能力：用户态程序无法直接读写 0x2E/0x4E 这类
// I/O 端口，必须由内核驱动代劳。这正是 Windows 上读取 Super I/O 芯片
// （风扇转速寄存器所在处）以及写入风扇 PWM 占空比的唯一途径。
type PawnIO struct {
	handle windows.Handle
}

// OpenPawnIO 打开 PawnIO 设备。
func OpenPawnIO() (*PawnIO, error) {
	name, err := windows.UTF16PtrFromString(pawnIODevicePath)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(
		name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("打开 PawnIO 设备失败（驱动未安装？）: %w", err)
	}
	return &PawnIO{handle: h}, nil
}

// Close 释放设备句柄。
func (p *PawnIO) Close() {
	if p.handle != 0 {
		_ = windows.CloseHandle(p.handle)
		p.handle = 0
	}
}

// LoadModule 把一个 PawnIO 模块（.bin）加载进驱动。
// 模块提供具体的能力（端口读写、SuperIO 寄存器访问等）。
func (p *PawnIO) LoadModule(data []byte) error {
	if len(data) == 0 {
		return errors.New("PawnIO 模块内容为空")
	}
	var returned uint32
	err := windows.DeviceIoControl(
		p.handle, pawnIODeviceType|ioctlPioLoadBin,
		&data[0], uint32(len(data)),
		nil, 0, &returned, nil,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_INITIALIZED) {
			return fmt.Errorf("该句柄已经加载过 PawnIO 模块（驱动限制：一个句柄只能加载一个模块，请另开一个句柄）: %w", err)
		}
		return fmt.Errorf("加载 PawnIO 模块失败: %w", err)
	}
	return nil
}

// Execute 调用驱动中已加载模块的一个函数。
//
// name 是模块里的函数名，args 是整数参数（按 int64 传参），
// outLen 是期望的返回值个数（按 int64 计）。
func (p *PawnIO) Execute(name string, args []int64, outLen int) ([]int64, error) {
	in := make([]byte, pawnIOFnNameLen+len(args)*8)
	copy(in, name)
	for i, a := range args {
		binary.LittleEndian.PutUint64(in[pawnIOFnNameLen+i*8:], uint64(a))
	}

	var out []byte
	if outLen > 0 {
		out = make([]byte, outLen*8)
	}

	var returned uint32
	var inPtr, outPtr *byte
	if len(in) > 0 {
		inPtr = &in[0]
	}
	if len(out) > 0 {
		outPtr = &out[0]
	}

	err := windows.DeviceIoControl(
		p.handle, pawnIODeviceType|ioctlPioExecFn,
		inPtr, uint32(len(in)),
		outPtr, uint32(len(out)), &returned, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("调用 PawnIO 函数 %s 失败: %w", name, err)
	}

	n := int(returned / 8)
	if n > outLen {
		n = outLen
	}
	res := make([]int64, n)
	for i := 0; i < n; i++ {
		res[i] = int64(binary.LittleEndian.Uint64(out[i*8:]))
	}
	return res, nil
}

// ReadPortByte 读取一个 I/O 端口（8 位）。
//
// 注意：模块的 ioctl_pio_inb 只允许访问"已发现 BAR 内的端口"或
// 当前配置端口（0x2E/0x2F、0x4E/0x4F）。访问硬件监控运行时的寄存器
// 之前必须先调用 FindBars() 填充白名单，否则会返回 ACCESS_DENIED。
func (p *PawnIO) ReadPortByte(port uint16) (byte, error) {
	res, err := p.Execute("ioctl_pio_inb", []int64{int64(port)}, 1)
	if err != nil {
		return 0, err
	}
	if len(res) == 0 {
		return 0, errors.New("ioctl_pio_inb 没有返回数据")
	}
	return byte(res[0]), nil
}

// WritePortByte 写入一个 I/O 端口（8 位）。
func (p *PawnIO) WritePortByte(port uint16, value byte) error {
	_, err := p.Execute("ioctl_pio_outb", []int64{int64(port), int64(value)}, 0)
	return err
}

// SelectSlot 选择 Super I/O 芯片所在的配置端口槽位。
//
// slot 0 对应 0x2E/0x2F，slot 1 对应 0x4E/0x4F。
// 该调用会重置模块内部的端口白名单，因此每次换槽位都要重新 FindBars。
func (p *PawnIO) SelectSlot(slot int) error {
	_, err := p.Execute("ioctl_select_slot", []int64{int64(slot)}, 0)
	if err != nil {
		return fmt.Errorf("选择 Super I/O 槽位 %d 失败: %w", slot, err)
	}
	return nil
}

// FindBars 扫描芯片所有逻辑设备的基址，填充驱动内部的端口白名单。
//
// 必须在芯片处于配置态、且芯片 ID 有效时调用；模块会遍历所有逻辑设备号
// 读取 0x60/0x62 基址寄存器。没有这一步，后续访问硬件监控运行时寄存器
// （基址 + 0x04/0x05/0x06）会一律被 PawnIO 拒绝。
//
// 该调用会改变当前选中的逻辑设备号，之后需要重新 Select。
func (p *PawnIO) FindBars() error {
	_, err := p.Execute("ioctl_find_bars", nil, 0)
	if err != nil {
		return fmt.Errorf("扫描芯片基址失败: %w", err)
	}
	return nil
}

// SuperIOReadByte 读取 Super I/O 配置态寄存器（8 位）。
//
// 走模块内部的 superio_inb：先写寄存器号到索引端口，再从数据端口读回，
// 整个过程在内核驱动里完成，不会与其它进程交错。
func (p *PawnIO) SuperIOReadByte(reg byte) (byte, error) {
	res, err := p.Execute("ioctl_superio_inb", []int64{int64(reg)}, 1)
	if err != nil {
		return 0, err
	}
	if len(res) == 0 {
		return 0, errors.New("ioctl_superio_inb 没有返回数据")
	}
	return byte(res[0]), nil
}

// SuperIOWriteByte 写入 Super I/O 配置态寄存器（8 位）。
func (p *PawnIO) SuperIOWriteByte(reg byte, value byte) error {
	_, err := p.Execute("ioctl_superio_outb", []int64{int64(reg), int64(value)}, 0)
	return err
}

// SuperIOReadWord 读取 Super I/O 配置态寄存器（16 位）。
//
// 字节序由 PawnIO 模块决定（高位在前），不同版本可能有差异。
// 需要确定的字节序时请改用 SuperIOReadByte 逐字节读取。
func (p *PawnIO) SuperIOReadWord(reg byte) (uint16, error) {
	res, err := p.Execute("ioctl_superio_inw", []int64{int64(reg)}, 1)
	if err != nil {
		return 0, err
	}
	if len(res) == 0 {
		return 0, errors.New("ioctl_superio_inw 没有返回数据")
	}
	return uint16(res[0]), nil
}

// ReadMSR 读取一个模型专用寄存器（MSR）。
//
// 需要先加载 IntelMSR 模块，且 CPU 必须是 Intel。模块内部有白名单，
// 只允许读取温度、功耗等相关寄存器。
func (p *PawnIO) ReadMSR(msr uint32) (uint64, error) {
	res, err := p.Execute("ioctl_read_msr", []int64{int64(msr)}, 1)
	if err != nil {
		return 0, fmt.Errorf("读取 MSR 0x%X 失败: %w", msr, err)
	}
	if len(res) == 0 {
		return 0, fmt.Errorf("读取 MSR 0x%X 没有返回数据", msr)
	}
	return uint64(res[0]), nil
}
