//go:build windows

package main

import (
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"
)

// Super I/O 芯片识别所需的配置端口常量。
const (
	chipIDRegister     = 0x20
	chipRevisionReg    = 0x21
	baseAddressReg     = 0x60
	deviceSelectReg    = 0x07
	ioSpaceLockReg     = 0x28
	hardwareMonitorLDN = 0x0B
)

// 运行时寄存器访问方式。
const (
	addressRegisterOffset = 0x05
	dataRegisterOffset    = 0x06
	bankSelectRegister    = 0x4E

	// EC 风格访问（NCT668x 系列）用的偏移。
	ecPageRegisterOffset = 0x04
	ecIndexRegisterOff   = 0x05
	ecDataRegisterOffset = 0x06
	ecPageSelectValue    = 0xFF
)

// savedControl 记录接管风扇前芯片的原始状态，用于交还硬件控制。
type savedControl struct {
	mode byte
	duty byte
}

// SuperIO 表示一台已识别的 Super I/O 芯片，提供寄存器读写与风扇调速。
type SuperIO struct {
	pawn       *PawnIO
	layout     superIOLayout
	chip       chipInfo
	chipID     uint16
	configPort uint16
	basePort   uint16
	isa        *isaBusMutex

	// slot 是配置端口所在的槽位（0x2E 对应 0，0x4E 对应 1）。
	slot int
	// unlock 是进入芯片配置态的解锁序列，重新初始化芯片时要复用。
	unlock func(*PawnIO, uint16) error

	mu    sync.Mutex
	saved map[int]savedControl
}

// ChipName 返回芯片型号名称。
func (s *SuperIO) ChipName() string { return s.chip.name }

// BasePort 返回硬件监控运行时的基址端口。
func (s *SuperIO) BasePort() uint16 { return s.basePort }

// ChannelCount 返回可控风扇通道数。
func (s *SuperIO) ChannelCount() int { return s.layout.channels }

// SupportsControl 报告该芯片的寄存器表是否已实现（能否调速）。
func (s *SuperIO) SupportsControl() bool {
	return s.chip.family != familyUnknown && len(s.layout.pwmRegs) > 0
}

// probeResult 记录一次 Super I/O 探测的完整过程，用于诊断输出。
type probeResult struct {
	Port       uint16
	Slot       int
	Unlock     string
	Unlocked   bool
	ChipID     byte
	Revision   byte
	ChipName   string
	Recognized bool
	BaseWord   uint16
	BaseBytes  [2]byte
	BasePort   uint16
	BaseValid  bool
	Note       string

	// unlock 是本次探测使用的解锁序列。重新初始化芯片时需要复用它。
	unlock func(*PawnIO, uint16) error
}

// String 把探测结果整理成一行易读的文本。
func (p probeResult) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "端口 0x%02X/%s 解锁=%v ", p.Port, p.Unlock, p.Unlocked)
	fmt.Fprintf(&b, "芯片 ID=0x%02X 修订=0x%02X ", p.ChipID, p.Revision)
	if p.Recognized {
		fmt.Fprintf(&b, "识别为 %s ", p.ChipName)
	} else {
		b.WriteString("未在支持列表中 ")
	}
	fmt.Fprintf(&b, "基址字节=[0x%02X 0x%02X] word=0x%04X", p.BaseBytes[0], p.BaseBytes[1], p.BaseWord)
	if p.BaseValid {
		fmt.Fprintf(&b, " → 基址 0x%04X", p.BasePort)
	}
	if p.Note != "" {
		fmt.Fprintf(&b, " (%s)", p.Note)
	}
	return b.String()
}

// ProbeSuperIO 在两个配置端口上完整走一遍探测流程，把每一步都记录下来。
//
// 这既是 DetectSuperIO 的底层实现，也是 -diag 命令的诊断数据来源。
func ProbeSuperIO(pawn *PawnIO) []probeResult {
	isa := newISABusMutex()
	if isa == nil {
		isa = &isaBusMutex{}
	}
	defer isa.Close()

	var results []probeResult

	for _, cp := range []uint16{0x2E, 0x4E} {
		slot := 0
		if cp == 0x4E {
			slot = 1
		}

		for _, unlock := range []struct {
			name string
			fn   func(*PawnIO, uint16) error
		}{
			{"Nuvoton/Winbond (0x87 x2)", enterConfigNuvoton},
			{"ITE (0x87,0x01,0x55)", enterConfigITE},
		} {
			res, responded := probeOnePort(pawn, isa, cp, slot, unlock.name, unlock.fn)
			results = append(results, res)
			if res.Recognized && res.BaseValid {
				// 已经找到受支持的芯片，不必再试其它端口或解锁序列。
				//
				// 提前结束还有另一层好处：ioctl_select_slot 会清空驱动内部的
				// 端口白名单，探测别的槽位会把已经扫描到的基址列表抹掉。
				return results
			}
			if responded {
				// 这个端口上已经有芯片应答，不必再试另一种解锁序列。
				break
			}
		}
	}
	return results
}

// probeOnePort 在一个配置端口上用一种解锁序列走一遍探测流程。
//
// 返回值第二个参数表示"芯片是否有应答"，用于决定要不要继续尝试其它序列。
func probeOnePort(pawn *PawnIO, isa *isaBusMutex, cp uint16, slot int,
	unlockName string, unlock func(*PawnIO, uint16) error) (probeResult, bool) {

	res := probeResult{Port: cp, Slot: slot, Unlock: unlockName, unlock: unlock}

	// 整个探测过程持有 ISA 总线锁，避免与 LibreHardwareMonitor 抢总线。
	if !isa.Lock(2000) {
		res.Note = "获取 ISA 总线锁超时"
		return res, false
	}
	defer isa.Unlock()

	if err := pawn.SelectSlot(slot); err != nil {
		res.Note = "选择槽位失败: " + err.Error()
		return res, false
	}
	if err := unlock(pawn, cp); err != nil {
		res.Note = "解锁失败: " + err.Error()
		return res, false
	}
	res.Unlocked = true

	id, err := pawn.SuperIOReadByte(chipIDRegister)
	if err != nil {
		exitConfig(pawn, cp)
		res.Note = "读芯片 ID 失败: " + err.Error()
		return res, false
	}
	rev, err := pawn.SuperIOReadByte(chipRevisionReg)
	if err != nil {
		exitConfig(pawn, cp)
		res.Note = "读修订号失败: " + err.Error()
		return res, false
	}
	res.ChipID = id
	res.Revision = rev

	if info, ok := lookupChip(id, rev); ok {
		res.ChipName = info.name
		res.Recognized = true
	} else if info, ok := lookupIT87Chip(id, rev); ok {
		res.ChipName = info.name
		res.Recognized = true
	}

	// 芯片 ID 无效：说明这个端口上没有芯片（或没进配置态）。
	if id == 0x00 || id == 0xFF {
		exitConfig(pawn, cp)
		res.Note = "芯片 ID 无效（未进入配置态？）"
		return res, false
	}

	// 扫描基址。必须在配置态下调用，且它会改变当前选中的逻辑设备号。
	if err := pawn.FindBars(); err != nil {
		exitConfig(pawn, cp)
		res.Note = "扫描基址失败: " + err.Error()
		return res, true
	}

	// 重新选中硬件监控逻辑设备，再读基址。
	if err := pawn.SuperIOWriteByte(deviceSelectReg, hardwareMonitorLDN); err != nil {
		exitConfig(pawn, cp)
		res.Note = "选择硬件监控逻辑设备失败: " + err.Error()
		return res, true
	}

	lo, err1 := pawn.SuperIOReadByte(baseAddressReg)
	hi, err2 := pawn.SuperIOReadByte(baseAddressReg + 1)
	word, err3 := pawn.SuperIOReadWord(baseAddressReg)
	if err1 != nil || err2 != nil || err3 != nil {
		exitConfig(pawn, cp)
		res.Note = fmt.Sprintf("读基址失败 (lo=%v hi=%v word=%v)", err1, err2, err3)
		return res, true
	}
	res.BaseBytes = [2]byte{lo, hi}
	res.BaseWord = word

	// 间隔 1ms 复读一次做校验。
	time.Sleep(time.Millisecond)
	lo2, _ := pawn.SuperIOReadByte(baseAddressReg)
	hi2, _ := pawn.SuperIOReadByte(baseAddressReg + 1)

	// 基址必须用与驱动完全一致的读法。
	//
	// PawnIO 模块里的 find_bars 用 superio_inw(0x60) 读基址，并把结果
	// 规范化后放进端口白名单；驱动只放行白名单里的端口。字节序一旦猜错，
	// 访问"基址 + 0x04/0x05/0x06"就会被以"拒绝访问"挡下。
	// 这里的 word 就是同一个 superio_inw 的结果，因此是唯一正确的候选。
	base := baseFromSuperIOWord(word)
	res.BasePort = base
	res.BaseValid = baseAddressValid(base)
	if !res.BaseValid {
		res.Note = fmt.Sprintf("基址 0x%04X 不符合常规（lo=0x%02X hi=0x%02X）", base, lo, hi)
	} else if lo2 != lo || hi2 != hi {
		// 复读不一致：芯片可能正在被别的程序改动，稍后重新武装时会再校验一次。
		res.Note = "基址两次读到的字节不一致"
	}

	exitConfig(pawn, cp)
	return res, true
}

// rearmSuperIO 为已经识别出的芯片重新填充驱动内部的端口白名单。
//
// 为什么必须做这一步：PawnIO 的 LpcIO 模块里，ioctl_select_slot 会先调用
// reset()，把已经扫描到的基址列表（g_bars）清空。探测过程要依次尝试
// 0x2E 与 0x4E 两个槽位，最后一次 SelectSlot 就把白名单抹掉了；
// 此时再去读硬件监控的运行时寄存器（基址 + 0x04/0x05/0x06），
// 驱动会以 STATUS_ACCESS_DENIED 拒绝。
//
// 所以选定芯片之后必须重新走一遍：
// 选槽位 → 进配置态 → 校验芯片 ID → 扫描基址 → 选硬件监控逻辑设备 → 退出配置态。
func rearmSuperIO(pawn *PawnIO, r *probeResult) error {
	if r.unlock == nil {
		return errors.New("缺少解锁序列")
	}
	if err := pawn.SelectSlot(r.Slot); err != nil {
		return err
	}
	// 先注册退出序列：即使解锁只成功了一半（写了一次 0x87），
	// 也要把芯片带回普通工作状态。往配置端口写 0xAA 在非配置态下无害。
	defer exitConfig(pawn, r.Port)
	if err := r.unlock(pawn, r.Port); err != nil {
		return fmt.Errorf("重新进入配置态失败: %w", err)
	}

	id, err := pawn.SuperIOReadByte(chipIDRegister)
	if err != nil {
		return fmt.Errorf("复读芯片 ID 失败: %w", err)
	}
	if id != r.ChipID {
		return fmt.Errorf("复读芯片 ID 得到 0x%02X，与探测时的 0x%02X 不一致", id, r.ChipID)
	}
	if err := pawn.FindBars(); err != nil {
		return fmt.Errorf("重新扫描基址失败: %w", err)
	}
	if err := pawn.SuperIOWriteByte(deviceSelectReg, hardwareMonitorLDN); err != nil {
		return fmt.Errorf("重新选择硬件监控逻辑设备失败: %w", err)
	}
	return nil
}

// verifyBase 校验当前基址能否读出合理的温度值。
func (s *SuperIO) verifyBase() bool {
	if !s.isa.Lock(200) {
		return false
	}
	defer s.isa.Unlock()
	return s.verifyBaseLocked()
}

// verifyBaseLocked 是 verifyBase 的无锁版本。
func (s *SuperIO) verifyBaseLocked() bool {
	if len(s.layout.tempSources) == 0 {
		return false
	}
	for _, ts := range s.layout.tempSources {
		v, err := s.readReg(ts.reg)
		if err != nil {
			return false
		}
		if v == 0x00 || v == 0xFF {
			continue
		}
		temp := float64(int8(v))
		if temp > 0 && temp < 125 {
			return true
		}
	}
	return false
}

// collectBaseCandidates 在配置态下枚举所有逻辑设备的基址。
//
// Super I/O 配置态寄存器的访问不走端口白名单，所以白名单被清空时依然能读。
func (s *SuperIO) collectBaseCandidates() []uint16 {
	if s.unlock == nil {
		return nil
	}
	if !s.isa.Lock(500) {
		return nil
	}
	defer s.isa.Unlock()

	if err := s.pawn.SelectSlot(s.slot); err != nil {
		return nil
	}
	if err := s.unlock(s.pawn, s.configPort); err != nil {
		return nil
	}
	defer exitConfig(s.pawn, s.configPort)

	seen := map[uint16]bool{}
	var out []uint16
	for ldn := 0; ldn < 0xFF; ldn++ {
		if err := s.pawn.SuperIOWriteByte(deviceSelectReg, byte(ldn)); err != nil {
			return out
		}
		// 与模块里的 select() 一样，回读确认逻辑设备号确实切换成功。
		got, err := s.pawn.SuperIOReadByte(deviceSelectReg)
		if err != nil || got != byte(ldn) {
			continue
		}
		word, err := s.pawn.SuperIOReadWord(baseAddressReg)
		if err != nil {
			return out
		}
		if word == 0x0000 || word == 0xFFFF {
			continue
		}
		base := baseFromSuperIOWord(word)
		if !baseAddressValid(base) || seen[base] {
			continue
		}
		seen[base] = true
		out = append(out, base)
	}
	return out
}

// findWorkingBase 在所有逻辑设备的基址里找一个能读出合理温度的。
//
// 硬件监控逻辑设备号 0x0B 是 Winbond/Nuvoton 的惯例，但个别板子并不遵循。
// 这里枚举所有逻辑设备的基址，重新武装驱动，然后逐个试读温度。
func (s *SuperIO) findWorkingBase(r *probeResult) (uint16, bool) {
	candidates := s.collectBaseCandidates()
	if len(candidates) == 0 {
		return 0, false
	}

	// 枚举过程调用过 SelectSlot，白名单已被清空，需要重新武装。
	if err := rearmSuperIO(s.pawn, r); err != nil {
		return 0, false
	}

	orig := s.basePort
	defer func() { s.basePort = orig }()

	for _, c := range candidates {
		if c == orig {
			continue
		}
		s.basePort = c
		if s.verifyBase() {
			return c, true
		}
	}
	return 0, false
}

// DetectSuperIO 探测 Super I/O 芯片。
//
// 流程与 LibreHardwareMonitor 一致：
// 进入配置态 → 读芯片 ID → 扫描基址（填充端口白名单）→
// 选硬件监控逻辑设备 → 读基址并校验 → 退出配置态。
func DetectSuperIO(pawn *PawnIO) (*SuperIO, error) {
	return detectSuperIOFrom(pawn, ProbeSuperIO(pawn))
}

// detectSuperIOFrom 从已有的探测结果构建 SuperIO 对象，避免重复探测。
func detectSuperIOFrom(pawn *PawnIO, results []probeResult) (*SuperIO, error) {
	var chosen *probeResult
	for i := range results {
		r := results[i]
		if !r.Unlocked {
			continue
		}
		if r.Recognized && r.BaseValid {
			if chosen == nil {
				chosen = &results[i]
			}
			continue
		}
	}

	if chosen == nil {
		return nil, describeProbeFailure(results)
	}

	isa := newISABusMutex()
	if isa == nil {
		isa = &isaBusMutex{}
	}

	info, ok := lookupChip(chosen.ChipID, chosen.Revision)
	if !ok {
		info, _ = lookupIT87Chip(chosen.ChipID, chosen.Revision)
	}

	sio := &SuperIO{
		pawn:       pawn,
		chip:       info,
		chipID:     uint16(chosen.ChipID)<<8 | uint16(chosen.Revision),
		configPort: chosen.Port,
		slot:       chosen.Slot,
		unlock:     chosen.unlock,
		basePort:   chosen.BasePort,
		isa:        isa,
		saved:      map[int]savedControl{},
	}
	sio.layout = layoutFor(info)

	if sio.chip.family == familyUnknown {
		return sio, fmt.Errorf("已识别 %s，但本程序尚未实现该芯片的寄存器表", info.name)
	}

	// 重新填充端口白名单。探测其它槽位时驱动把它清空了，
	// 不做这一步的话读运行时寄存器只会得到"拒绝访问"。
	if err := rearmSuperIO(pawn, chosen); err != nil {
		return sio, fmt.Errorf("重新初始化芯片失败: %w", err)
	}

	// 校验基址。若 LDN 0x0B 上读不到合理温度，说明这块板子的硬件监控
	// 不在常规逻辑设备上，换用其它逻辑设备的基址再试。
	if !sio.verifyBase() {
		if alt, ok := sio.findWorkingBase(chosen); ok {
			sio.basePort = alt
		}
	}

	// 芯片初始化：NCT668x 需要打开温度采集与电压监控。
	sio.initChip()
	return sio, nil
}

// describeProbeFailure 把探测过程整理成一段可读的失败说明。
func describeProbeFailure(results []probeResult) error {
	var b strings.Builder
	b.WriteString("没有找到受支持的 Super I/O 芯片")

	var unknown []probeResult
	for _, r := range results {
		if r.Unlocked && r.ChipID != 0x00 && r.ChipID != 0xFF && !r.Recognized {
			unknown = append(unknown, r)
		}
	}
	if len(unknown) > 0 {
		b.WriteString("；检测到未知芯片：")
		for i, r := range unknown {
			if i > 0 {
				b.WriteString("；")
			}
			fmt.Fprintf(&b, "端口 0x%02X 上 ID=0x%02X 修订=0x%02X", r.Port, r.ChipID, r.Revision)
		}
		b.WriteString("（需要补充该型号的寄存器表）")
		return errors.New(b.String())
	}
	return errors.New(b.String())
}

// enterConfigNuvoton 是 Nuvoton / Winbond / Fintek 的解锁序列。
//
// 关键点：0x87 要连续两次写到"配置端口本身"（0x2E 或 0x4E），
// 而不是分别写到配置端口和数据端口。
func enterConfigNuvoton(p *PawnIO, cp uint16) error {
	if err := p.WritePortByte(cp, 0x87); err != nil {
		return err
	}
	return p.WritePortByte(cp, 0x87)
}

// enterConfigITE 是 ITE IT87 系列的解锁序列。
func enterConfigITE(p *PawnIO, cp uint16) error {
	seq := []byte{0x87, 0x01, 0x55}
	for _, b := range seq {
		if err := p.WritePortByte(cp, b); err != nil {
			return err
		}
	}
	tail := byte(0xAA)
	if cp != 0x4E {
		tail = 0x55
	}
	return p.WritePortByte(cp, tail)
}

// exitConfig 退出配置态。
func exitConfig(p *PawnIO, cp uint16) {
	_ = p.WritePortByte(cp, 0xAA)
}

// initChip 做芯片上电后必要的初始化。
//
// NCT668x 需要打开温度采集位，并使能 SIO 电压监控，
// 否则读出来的温度恒为 0。
func (s *SuperIO) initChip() {
	if s.chip.family != familyNCT668x {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.isa.Lock(200) {
		return
	}
	defer s.isa.Unlock()

	// 打开温度采集（bit 0x80）。
	if v, err := s.readReg(0x180); err == nil && v&0x80 == 0 {
		_ = s.writeReg(0x180, v|0x80)
	}
	// 使能 SIO 电压监控。
	for i := 0; i < 5; i++ {
		_ = s.writeReg(uint16(0x1BB+i), byte(0x61+i))
	}
}

// readReg 读取一个运行时寄存器。
//
// 调用方必须已经持有 ISA 总线锁，这样一整批读数（转速的高低字节等）
// 不会被别的进程插进来改写。
func (s *SuperIO) readReg(addr uint16) (byte, error) {
	if s.layout.ecSpace {
		return s.ecAccess(byte(addr>>8), byte(addr&0xFF), 0, false)
	}
	bank := byte(addr >> 8)
	reg := byte(addr & 0xFF)
	base := s.basePort
	if err := s.pawn.WritePortByte(base+addressRegisterOffset, bankSelectRegister); err != nil {
		return 0, err
	}
	if err := s.pawn.WritePortByte(base+dataRegisterOffset, bank); err != nil {
		return 0, err
	}
	if err := s.pawn.WritePortByte(base+addressRegisterOffset, reg); err != nil {
		return 0, err
	}
	return s.pawn.ReadPortByte(base + dataRegisterOffset)
}

// writeReg 写入一个运行时寄存器。调用方必须已持有 ISA 总线锁。
func (s *SuperIO) writeReg(addr uint16, val byte) error {
	if s.layout.ecSpace {
		_, err := s.ecAccess(byte(addr>>8), byte(addr&0xFF), val, true)
		return err
	}
	bank := byte(addr >> 8)
	reg := byte(addr & 0xFF)
	base := s.basePort
	if err := s.pawn.WritePortByte(base+addressRegisterOffset, bankSelectRegister); err != nil {
		return err
	}
	if err := s.pawn.WritePortByte(base+dataRegisterOffset, bank); err != nil {
		return err
	}
	if err := s.pawn.WritePortByte(base+addressRegisterOffset, reg); err != nil {
		return err
	}
	return s.pawn.WritePortByte(base+dataRegisterOffset, val)
}

// ecAccess 走 EC 风格访问运行时寄存器（页 / 索引 / 数据）。
func (s *SuperIO) ecAccess(page, index, value byte, write bool) (byte, error) {
	base := s.basePort

	// 等待总线访问权。芯片用 base+0x04 存放访问标志，0xFF 表示空闲。
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		access, err := s.pawn.ReadPortByte(base + ecPageRegisterOffset)
		if err != nil {
			return 0, err
		}
		if access == ecPageSelectValue || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	if err := s.pawn.WritePortByte(base+ecPageRegisterOffset, page); err != nil {
		_ = s.pawn.WritePortByte(base+ecPageRegisterOffset, ecPageSelectValue)
		return 0, err
	}
	if err := s.pawn.WritePortByte(base+ecIndexRegisterOff, index); err != nil {
		_ = s.pawn.WritePortByte(base+ecPageRegisterOffset, ecPageSelectValue)
		return 0, err
	}

	var result byte
	if write {
		if err := s.pawn.WritePortByte(base+ecDataRegisterOffset, value); err != nil {
			_ = s.pawn.WritePortByte(base+ecPageRegisterOffset, ecPageSelectValue)
			return 0, err
		}
		result = value
	} else {
		v, err := s.pawn.ReadPortByte(base + ecDataRegisterOffset)
		if err != nil {
			_ = s.pawn.WritePortByte(base+ecPageRegisterOffset, ecPageSelectValue)
			return 0, err
		}
		result = v
	}

	_ = s.pawn.WritePortByte(base+ecPageRegisterOffset, ecPageSelectValue)
	return result, nil
}

// Temperatures 读取芯片内置的全部温度传感器。
func (s *SuperIO) Temperatures() ([]hwTemp, error) {
	if len(s.layout.tempSources) == 0 {
		return nil, errors.New("该芯片没有已知的温度传感器寄存器表")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.isa.Lock(2000) {
		return nil, errors.New("获取 ISA 总线锁超时")
	}
	defer s.isa.Unlock()

	out := make([]hwTemp, 0, len(s.layout.tempSources))
	for _, ts := range s.layout.tempSources {
		v, err := s.readReg(ts.reg)
		if err != nil {
			return nil, err
		}
		temp := float64(int8(v))
		if ts.halfBit != noHalfBit {
			half, err := s.readReg(ts.halfReg)
			if err != nil {
				return nil, err
			}
			bit := (half >> ts.halfBit) & 0x01
			if ts.halfLSB {
				// 整数部分以 2 度为单位，低位是小数位。
				val := int(int8(v))<<1 | int(bit)
				temp = float64(val) / 2
			} else {
				temp += 0.5 * float64(bit)
			}
		}
		// 明显不合理的值直接丢弃（未接探头时读到 0 或 127）。
		if temp <= 0 || temp > 125 {
			continue
		}
		out = append(out, hwTemp{Name: ts.name, Value: temp})
	}
	if len(out) == 0 {
		return nil, errors.New("没有读到有效的温度读数")
	}
	return out, nil
}

// Fans 读取全部风扇通道的转速与占空比。
func (s *SuperIO) Fans() ([]hwFan, error) {
	if len(s.layout.rpmRegs) == 0 {
		return nil, errors.New("该芯片没有已知的风扇转速寄存器表")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.isa.Lock(2000) {
		return nil, errors.New("获取 ISA 总线锁超时")
	}
	defer s.isa.Unlock()

	names := nct679xFanNames
	if s.layout.kind == "nct668x" {
		names = nct668xFanNames
	}

	// 有占空比寄存器的通道才是可调速通道（控制通道）；
	// 多出来的转速寄存器只是只读测速口，对应不上任何 PWM 输出。
	controlChans := len(s.layout.dutyRegs)

	out := make([]hwFan, 0, len(s.layout.rpmRegs))
	for i, reg := range s.layout.rpmRegs {
		high, err := s.readReg(reg)
		if err != nil {
			return nil, err
		}
		low, err := s.readReg(reg + 1)
		if err != nil {
			return nil, err
		}

		var rpm float64
		if containsU16(s.layout.countRegs, reg) {
			rpm = rpmFrom13Bit(high, low)
		} else {
			raw := int(high)<<8 | int(low)
			if raw == 0xFFFF || raw < 0 {
				rpm = 0
			} else {
				rpm = float64(raw)
			}
		}
		if rpm > 30000 { // 异常读数保护
			rpm = 0
		}

		channel := -1
		if i < controlChans {
			channel = i
		}

		var name string
		if channel >= 0 {
			base := fmt.Sprintf("风扇 %d", channel+1)
			if channel < len(names) {
				base = names[channel]
			}
			// 通道号放在最前面：现场那台机器上"CPU 风扇"位置其实没接风扇，
			// 只有通道号能准确对应到界面里的下拉选择。
			name = fmt.Sprintf("通道 %d · %s", channel, base)
		} else {
			name = fmt.Sprintf("附加测速 %d（只读）", i-controlChans+1)
		}

		f := hwFan{Name: name, RPM: rpm, Channel: channel, Index: i}
		if channel >= 0 {
			if duty, err := s.readReg(s.layout.dutyRegs[channel]); err == nil {
				f.Duty = math.Round(float64(duty) / 2.55)
				f.HasDuty = true
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// readModeBits 读取当前的风扇控制模式寄存器，返回手动模式通道的位掩码。
//
// 该方法自行加锁，可从诊断代码这类非采集路径安全调用。
func (s *SuperIO) readModeBits() (byte, error) {
	if len(s.layout.modeRegs) == 0 {
		return 0, errors.New("该芯片没有模式寄存器")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.isa.Lock(2000) {
		return 0, errors.New("获取 ISA 总线锁超时")
	}
	defer s.isa.Unlock()

	return s.readReg(s.layout.modeRegs[0])
}

// SetManual 把指定风扇通道切换到手动模式并写入占空比。
//
// percent 是 0-100 的占空比百分比。为安全起见不低于 minSafeDutyPercent。
func (s *SuperIO) SetManual(channel int, percent float64) error {
	if channel < 0 || channel >= s.layout.channels {
		return fmt.Errorf("风扇通道 %d 超出范围（0-%d）", channel, s.layout.channels-1)
	}
	if len(s.layout.pwmRegs) == 0 {
		return errors.New("该芯片没有可写的风扇占空比寄存器")
	}
	if percent < minSafeDutyPercent {
		percent = minSafeDutyPercent
	}
	if percent > 100 {
		percent = 100
	}
	duty := byte(math.Round(percent * 2.55))

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.isa.Lock(2000) {
		return errors.New("获取 ISA 总线锁超时")
	}
	defer s.isa.Unlock()

	if _, ok := s.saved[channel]; !ok {
		mode, err := s.readReg(s.layout.modeRegs[channel])
		if err != nil {
			return fmt.Errorf("读取风扇 %d 模式寄存器失败: %w", channel, err)
		}
		cur, err := s.readReg(s.layout.pwmRegs[channel])
		if err != nil {
			return fmt.Errorf("读取风扇 %d 占空比寄存器失败: %w", channel, err)
		}
		s.saved[channel] = savedControl{mode: mode, duty: cur}

		// 把"接管前的原始值"落到磁盘：进程被强杀或崩溃时来不及还原，
		// 芯片会一直停在手动模式上（过温保护也随之失效）。
		// 下次启动时凭这个还原点把它恢复回去。
		entry := fanRestoreEntry{Channel: channel, Mode: mode, Duty: cur}
		if err := addFanRestoreEntry(fanRestorePointPath(), entry); err != nil {
			log.Printf("[fan] 保存风扇还原点失败: %v", err)
		}
	}

	if s.layout.ecSpace {
		// NCT668x：先向芯片申请访问权，再改模式与占空比。
		if err := s.writeReg(s.layout.requestReg, 0x80); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)

		mode, err := s.readReg(s.layout.modeRegs[channel])
		if err != nil {
			return err
		}
		mode |= 1 << uint(channel)
		if err := s.writeReg(s.layout.modeRegs[channel], mode); err != nil {
			return err
		}
		if err := s.writeReg(s.layout.pwmRegs[channel], duty); err != nil {
			return err
		}
		// 联想 ThinkCentre (M720q/M920q) 专有 EC 固件特性：
		// 写入 0x40 会直接被 EC 判定为交还 BIOS 自动控制；
		// 锁定手动接管时，完成握手字节必须写入 0x00！
		if err := s.writeReg(s.layout.requestReg, 0x00); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
		s.verifyDuty(channel, duty)
		return nil
	}

	// 传统布局：先写占空比再切手动，避免中途失败时风扇失去目标转速。
	if err := s.writeReg(s.layout.pwmRegs[channel], duty); err != nil {
		return err
	}
	if err := s.writeReg(s.layout.modeRegs[channel], 0); err != nil {
		return err
	}
	s.verifyDuty(channel, duty)
	return nil
}

// verifyDuty 读回占空比寄存器，检查刚才的写入是否真的生效了。
//
// 写入没有报错但寄存器没变，通常意味着这个通道没有接风扇，
// 或者该通道的 PWM 输出被 BIOS 固定住了。日志里记一笔，方便现场排查。
func (s *SuperIO) verifyDuty(channel int, want byte) {
	if channel < 0 || channel >= len(s.layout.dutyRegs) {
		return
	}
	got, err := s.readReg(s.layout.dutyRegs[channel])
	if err != nil {
		return
	}
	wantPct := float64(want) / 2.55
	gotPct := float64(got) / 2.55
	if diff := wantPct - gotPct; diff > 3 || diff < -3 {
		log.Printf("[fan] 通道 %d 占空比写入未生效：期望 %.0f%%，芯片读回 %.0f%%（该通道可能没接风扇）",
			channel, wantPct, gotPct)
	}
}

// Restore 把指定通道交还硬件自动控制，并恢复原始寄存器值。
func (s *SuperIO) Restore(channel int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sv, ok := s.saved[channel]
	if !ok {
		return nil
	}

	if !s.isa.Lock(2000) {
		return errors.New("获取 ISA 总线锁超时")
	}
	defer s.isa.Unlock()

	var err error
	if s.layout.ecSpace {
		// NCT668x：进入握手访问态
		if err = s.writeReg(s.layout.requestReg, 0x80); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)

		mode, rerr := s.readReg(s.layout.modeRegs[channel])
		if rerr != nil {
			err = rerr
		} else {
			// NCT668x 的模式寄存器是所有通道共用的，每通道占一位。
			// 只把本通道那一位恢复成接管前的值，其余通道的位不能动——
			// 那些位可能是 BIOS 自己设的。
			bit := byte(1) << uint(channel)
			mode = (mode &^ bit) | (sv.mode & bit)
			err = s.writeReg(s.layout.modeRegs[channel], mode)
		}
		if err == nil {
			err = s.writeReg(s.layout.pwmRegs[channel], sv.duty)
		}
		if werr := s.writeReg(s.layout.requestReg, 0x40); werr != nil && err == nil {
			err = werr
		}
		time.Sleep(50 * time.Millisecond)
	} else {
		if err = s.writeReg(s.layout.modeRegs[channel], sv.mode); err == nil {
			err = s.writeReg(s.layout.pwmRegs[channel], sv.duty)
		}
	}

	if err == nil {
		delete(s.saved, channel)
		// 这个通道已经交还硬件，不需要再留着还原点了。
		if rerr := removeFanRestoreEntry(fanRestorePointPath(), channel); rerr != nil {
			log.Printf("[fan] 清除风扇还原点失败: %v", rerr)
		}
	}
	return err
}

// RestoreLeftover 还原上一次进程异常退出时残留的手动模式。
//
// 正常退出时程序会调用 Restore 交还硬件；但进程被强杀（任务管理器结束、
// 服务被杀）时那一步来不及执行，芯片会停留在手动模式，
// 而过温保护也跟着失效——CPU 再热也不会自动提速。
// 这个方法在启动时调用，凭磁盘上的还原点把芯片恢复到安全状态。
func (s *SuperIO) RestoreLeftover() error {
	rp, ok, err := loadFanRestorePoint(fanRestorePointPath())
	if err != nil {
		return fmt.Errorf("读取风扇还原点失败: %w", err)
	}
	if !ok || len(rp.Entries) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.isa.Lock(2000) {
		return errors.New("获取 ISA 总线锁超时")
	}
	defer s.isa.Unlock()

	for _, e := range rp.Entries {
		if e.Channel < 0 || e.Channel >= s.layout.channels {
			continue
		}
		if err := s.restoreChannelLocked(e.Channel, e.Mode, e.Duty); err != nil {
			return fmt.Errorf("还原风扇 %d 失败: %w", e.Channel, err)
		}
		log.Printf("[fan] 已还原上次异常退出残留的手动模式：通道 %d", e.Channel)
	}

	return clearFanRestorePoint(fanRestorePointPath())
}

// restoreChannelLocked 把单个通道恢复到指定的模式与占空比。
// 调用方必须已持有 SuperIO 锁与 ISA 总线锁。
func (s *SuperIO) restoreChannelLocked(channel int, mode, duty byte) error {
	if s.layout.ecSpace {
		if err := s.writeReg(s.layout.requestReg, 0x80); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)

		cur, err := s.readReg(s.layout.modeRegs[channel])
		if err != nil {
			_ = s.writeReg(s.layout.requestReg, 0x40)
			return err
		}
		bit := byte(1) << uint(channel)
		cur = (cur &^ bit) | (mode & bit)
		if err := s.writeReg(s.layout.modeRegs[channel], cur); err != nil {
			_ = s.writeReg(s.layout.requestReg, 0x40)
			return err
		}
		if err := s.writeReg(s.layout.pwmRegs[channel], duty); err != nil {
			_ = s.writeReg(s.layout.requestReg, 0x40)
			return err
		}
		if err := s.writeReg(s.layout.requestReg, 0x40); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
		return nil
	}

	if err := s.writeReg(s.layout.modeRegs[channel], mode); err != nil {
		return err
	}
	return s.writeReg(s.layout.pwmRegs[channel], duty)
}

// Duty 读取当前占空比百分比。
func (s *SuperIO) Duty(channel int) (float64, bool) {
	if channel < 0 || channel >= len(s.layout.dutyRegs) {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isa.Lock(2000) {
		return 0, false
	}
	defer s.isa.Unlock()
	v, err := s.readReg(s.layout.dutyRegs[channel])
	if err != nil {
		return 0, false
	}
	return float64(v) / 2.55, true
}

// Managed 报告该通道当前是否处于我们的手动控制之下。
func (s *SuperIO) Managed(channel int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.saved[channel]
	return ok
}
