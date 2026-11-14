package main

import (
	"math"
	"testing"
)

// TestLookupChipNuvoton 覆盖 Nuvoton 芯片识别表。
//
// 这张表曾经漏掉 NCT6686D 的 0x41 修订号，导致 M720q 这类机器
// 完全探测不到芯片 —— 表的内容必须逐条与 LibreHardwareMonitor 对齐。
func TestLookupChipNuvoton(t *testing.T) {
	cases := []struct {
		id, rev byte
		want    string
		family  chipFamily
	}{
		{0xD4, 0x40, "Nuvoton NCT6686D", familyNCT668x},
		{0xD4, 0x41, "Nuvoton NCT6686D", familyNCT668x},
		{0xC7, 0x32, "Nuvoton NCT6683D", familyNCT668x},
		{0xD5, 0x92, "Nuvoton NCT6687D", familyNCT668x},
		{0xD4, 0x2B, "Nuvoton NCT6798D", familyNCT679x},
		{0xD4, 0x23, "Nuvoton NCT6796D", familyNCT679x},
		{0xD4, 0x2A, "Nuvoton NCT6796DR", familyNCT679x},
		{0xD4, 0x51, "Nuvoton NCT6797D", familyNCT679x},
		{0xD8, 0x02, "Nuvoton NCT6799D", familyNCT679x},
		{0xD8, 0x06, "Nuvoton NCT6701D", familyNCT679x},
		{0xD3, 0x52, "Nuvoton NCT6795D", familyNCT679x},
		{0xD1, 0x21, "Nuvoton NCT6793D", familyNCT679x},
		{0xC8, 0x03, "Nuvoton NCT6791D", familyNCT679x},
		{0xC9, 0x11, "Nuvoton NCT6792D", familyNCT679x},
		{0xC9, 0x13, "Nuvoton NCT6792DA", familyNCT679x},
		{0xB4, 0x70, "Nuvoton NCT6771F", familyNCT679x},
		{0xC5, 0x60, "Nuvoton NCT6779D", familyNCT679x},
	}

	for _, c := range cases {
		info, ok := lookupChip(c.id, c.rev)
		if !ok {
			t.Errorf("lookupChip(0x%02X, 0x%02X) 未识别，期望 %s", c.id, c.rev, c.want)
			continue
		}
		if info.name != c.want {
			t.Errorf("lookupChip(0x%02X, 0x%02X) = %q，期望 %q", c.id, c.rev, info.name, c.want)
		}
		if info.family != c.family {
			t.Errorf("lookupChip(0x%02X, 0x%02X) 家族 = %v，期望 %v", c.id, c.rev, info.family, c.family)
		}
	}
}

// TestLookupChipUnknown 确认未知芯片不会被误识别。
func TestLookupChipUnknown(t *testing.T) {
	for _, c := range []struct{ id, rev byte }{
		{0x00, 0x00}, {0xFF, 0xFF}, {0xD4, 0x00}, {0xD4, 0xFF}, {0x12, 0x34},
	} {
		if _, ok := lookupChip(c.id, c.rev); ok {
			t.Errorf("lookupChip(0x%02X, 0x%02X) 不应该识别出芯片", c.id, c.rev)
		}
	}
}

// TestLookupIT87Chip 确认 ITE 芯片按 16 位 ID 识别。
func TestLookupIT87Chip(t *testing.T) {
	info, ok := lookupIT87Chip(0x87, 0x28)
	if !ok || info.name != "ITE IT8728F" {
		t.Errorf("lookupIT87Chip(0x87, 0x28) = %q, %v，期望 ITE IT8728F", info.name, ok)
	}
	if _, ok := lookupIT87Chip(0xD4, 0x40); ok {
		t.Error("Nuvoton 的 ID 不应被当成 ITE 芯片")
	}
}

// TestLayoutForNCT668x 确认 M720q 所用芯片走的是 NCT668x 布局。
func TestLayoutForNCT668x(t *testing.T) {
	info, _ := lookupChip(0xD4, 0x40)
	layout := layoutFor(info)
	if layout.kind != "nct668x" {
		t.Fatalf("布局类型 = %q，期望 nct668x", layout.kind)
	}
	if !layout.ecSpace {
		t.Error("NCT668x 必须使用 EC 风格访问")
	}
	if layout.requestReg != 0xA01 {
		t.Errorf("访问请求寄存器 = 0x%03X，期望 0xA01", layout.requestReg)
	}
	if layout.modeRegs[0] != 0xA00 || layout.modeRegs[7] != 0xA00 {
		t.Errorf("NCT668x 的 8 个通道应当共用 0xA00 这一个模式寄存器，得到 %v", layout.modeRegs)
	}
}

// TestLayoutNCT668xRegisters 核对 NCT668x 的关键寄存器。
//
// 这些数字一旦错位，风扇会失控或温度恒为 0，必须逐一对照
// LibreHardwareMonitor 的 Nct677X.cs。
func TestLayoutNCT668xRegisters(t *testing.T) {
	l := layoutNCT668x

	// 温度传感器：0x100 起步、步长 2、共 11 个，第一个是 CPU。
	if len(l.tempSources) != 11 {
		t.Fatalf("温度传感器数量 = %d，期望 11", len(l.tempSources))
	}
	if l.tempSources[0].reg != 0x100 || l.tempSources[0].name != "CPU" {
		t.Errorf("第一个温度传感器 = %+v，期望 CPU @ 0x100", l.tempSources[0])
	}
	if l.tempSources[10].reg != 0x114 {
		t.Errorf("最后一个温度传感器寄存器 = 0x%03X，期望 0x114", l.tempSources[10].reg)
	}
	for i, ts := range l.tempSources {
		want := uint16(0x100 + i*2)
		if ts.reg != want {
			t.Errorf("温度传感器 %d 寄存器 = 0x%03X，期望 0x%03X", i, ts.reg, want)
		}
	}

	// 风扇转速：0x140 起步、步长 2，共 16 路测速寄存器。
	if len(l.rpmRegs) != 16 {
		t.Fatalf("转速寄存器数量 = %d，期望 16", len(l.rpmRegs))
	}
	for i := 0; i < 16; i++ {
		want := uint16(0x140 + i*2)
		if l.rpmRegs[i] != want {
			t.Errorf("转速寄存器 %d = 0x%03X，期望 0x%03X", i, l.rpmRegs[i], want)
		}
	}

	// 占空比读取：0x160 起步、步长 1、共 8 个。
	if len(l.dutyRegs) != 8 {
		t.Fatalf("占空比寄存器数量 = %d，期望 8", len(l.dutyRegs))
	}
	for i, r := range l.dutyRegs {
		if r != uint16(0x160+i) {
			t.Errorf("占空比寄存器 %d = 0x%03X，期望 0x%03X", i, r, 0x160+i)
		}
	}

	// 占空比写入：0xA28 起步、步长 1、共 8 个。
	if len(l.pwmRegs) != 8 {
		t.Fatalf("PWM 寄存器数量 = %d，期望 8", len(l.pwmRegs))
	}
	for i, r := range l.pwmRegs {
		if r != uint16(0xA28+i) {
			t.Errorf("PWM 寄存器 %d = 0x%03X，期望 0x%03X", i, r, 0xA28+i)
		}
	}

	// 手动模式：8 个通道共用 0xA00，靠 bit 区分。
	for i, r := range l.modeRegs {
		if r != 0xA00 {
			t.Errorf("模式寄存器 %d = 0x%03X，期望 0xA00", i, r)
		}
	}
}

// TestLayoutNCT679xRegisters 核对 NCT679x 的关键寄存器。
func TestLayoutNCT679xRegisters(t *testing.T) {
	l := layoutNCT679x
	if len(l.modeRegs) != 7 || len(l.pwmRegs) != 7 || len(l.dutyRegs) != 7 {
		t.Fatalf("NCT679x 的通道数不一致: mode=%d pwm=%d duty=%d",
			len(l.modeRegs), len(l.pwmRegs), len(l.dutyRegs))
	}
	if l.modeRegs[0] != 0x102 || l.modeRegs[6] != 0xB02 {
		t.Errorf("模式寄存器 = %v", l.modeRegs)
	}
	if l.pwmRegs[0] != 0x109 || l.pwmRegs[6] != 0xB09 {
		t.Errorf("PWM 寄存器 = %v", l.pwmRegs)
	}
	if l.ecSpace {
		t.Error("NCT679x 用 bank 选择风格访问，不是 EC 风格")
	}
}

// TestNormalizeBaseAddress 确认基址修正与合法性校验。
func TestNormalizeBaseAddress(t *testing.T) {
	cases := []struct {
		in   uint16
		want uint16
	}{
		{0x0290, 0x0290},
		{0x0295, 0x0290}, // 有些 Fintek 芯片的偏移已经加进去了
		{0x0460, 0x0460},
		{0x0465, 0x0460},
	}
	for _, c := range cases {
		if got := normalizeBaseAddress(c.in); got != c.want {
			t.Errorf("normalizeBaseAddress(0x%04X) = 0x%04X，期望 0x%04X", c.in, got, c.want)
		}
	}
}

func TestBaseAddressValid(t *testing.T) {
	valid := []uint16{0x0290, 0x02B0, 0x0460, 0x0295}
	for _, a := range valid {
		if !baseAddressValid(normalizeBaseAddress(a)) {
			t.Errorf("基址 0x%04X 应当合法", a)
		}
	}
	invalid := []uint16{0x0000, 0x0020, 0x00FF, 0x0291, 0x9002, 0xFFFF}
	for _, a := range invalid {
		if baseAddressValid(normalizeBaseAddress(a)) {
			t.Errorf("基址 0x%04X 应当非法", a)
		}
	}
}

// TestRPMFrom13Bit 确认 13 位计数器的换算。
func TestRPMFrom13Bit(t *testing.T) {
	// count = (high << 5) | (low & 0x1F)，转速 = 1.35e6 / count。
	// count = 0x100 → high=0x08, low=0x00
	got := rpmFrom13Bit(0x08, 0x00)
	want := 1.35e6 / 256.0
	if math.Abs(got-want) > 0.5 {
		t.Errorf("rpmFrom13Bit(0x08, 0x00) = %.2f，期望 %.2f", got, want)
	}

	// 计数太小（低于 minFanCount）或溢出时返回 0。
	if v := rpmFrom13Bit(0x00, 0x05); v != 0 {
		t.Errorf("计数过小时应返回 0，得到 %v", v)
	}
	if v := rpmFrom13Bit(0xFF, 0xFF); v != 0 {
		t.Errorf("计数溢出时应返回 0，得到 %v", v)
	}
}

// TestContainsU16 确认小工具函数。
func TestContainsU16(t *testing.T) {
	list := []uint16{0x4B0, 0x4B2, 0x4CC}
	if !containsU16(list, 0x4CC) {
		t.Error("应包含 0x4CC")
	}
	if containsU16(list, 0x4B1) {
		t.Error("不应包含 0x4B1")
	}
}

// TestBaseFromSuperIOWord 固化实机验证过的字节序与基址。
//
// 这条测试守着 v2.2.0 修掉的那个 bug 的另一半根因：
//
// PawnIO 模块里 find_bars 用 superio_inw(0x60) 读基址，把结果规范化后放进
// 驱动内部的端口白名单；驱动只放行白名单里的端口。因此基址必须用与
// superio_inw 完全一致的读法算出来，字节序一旦猜错，访问硬件监控寄存器
// 就会一律得到"拒绝访问"。
//
// 实机数据（NEC8 / M720q，Nuvoton NCT6686D，配置端口 0x2E，
// 芯片 ID 0xD4 修订 0x41，硬件监控逻辑设备 0x0B）：
//
//	寄存器 0x60 = 0x0A
//	寄存器 0x61 = 0x20
//	superio_inw(0x60) = (0x0A << 8) | 0x20 = 0x0A20
//
// 运行时基址因此是 0x0A20。
func TestBaseFromSuperIOWord(t *testing.T) {
	const (
		lo = 0x0A // 寄存器 0x60
		hi = 0x20 // 寄存器 0x61
	)

	// superio_inw 的字节序是"高位在前"。
	word := uint16(lo)<<8 | uint16(hi)
	if word != 0x0A20 {
		t.Fatalf("superio_inw 字节序不符：得到 0x%04X，期望 0x0A20", word)
	}

	base := baseFromSuperIOWord(word)
	if base != 0x0A20 {
		t.Fatalf("基址不对：得到 0x%04X，期望 0x0A20", base)
	}
	if !baseAddressValid(base) {
		t.Fatalf("基址 0x%04X 没通过合法性校验", base)
	}

	// 驱动只放行"基址 & 0xFFF8"这一个 8 字节块里的端口。
	// 硬件监控用到的三个端口都必须落在块内，否则读不到数据。
	const (
		ecPageRegisterOffset  = 0x04
		ecIndexRegisterOffset = 0x05
		ecDataRegisterOffset  = 0x06
	)
	for _, off := range []uint16{ecPageRegisterOffset, ecIndexRegisterOffset, ecDataRegisterOffset} {
		port := base + off
		if port&0xFFF8 != base&0xFFF8 {
			t.Errorf("端口 0x%04X 落在基址 0x%04X 的 8 字节块之外，驱动会拒绝访问",
				port, base)
		}
	}

	// 反过来验证：小端拼法得到 0x200A，通不过合法性校验，
	// 说明它不可能是这块板子的真实基址。
	littleEndian := uint16(lo) | uint16(hi)<<8
	if baseAddressValid(normalizeBaseAddress(littleEndian)) {
		t.Errorf("小端拼法 0x%04X 不该通过合法性校验", littleEndian)
	}
}

// TestTempSourceHalfRegisters 保证每个温度源的小数位寄存器都填对了。
//
// 踩过的坑：NCT668x 的温度源最初只写了 reg，小数位字段留空。
// 程序于是去读寄存器 0x000，并给温度平白加上 0 或 0.5 度。
//
// LibreHardwareMonitor 的 NCT668x 分支是硬编码的：
//
//	value = (sbyte)ReadByte(reg);
//	half  = (ReadByte(reg + 1) >> 7) & 0x1;
//
// 也就是说小数位固定在下一个寄存器的第 7 位。
func TestTempSourceHalfRegisters(t *testing.T) {
	layouts := map[string]superIOLayout{
		"NCT668x":    layoutNCT668x,
		"NCT679x":    layoutNCT679x,
		"NCT679xAlt": layoutNCT679xAlt,
		"NCT679xD":   layoutNCT679xD,
	}

	for name, l := range layouts {
		for _, ts := range l.tempSources {
			if ts.halfBit == noHalfBit {
				continue
			}
			if ts.halfReg == 0 {
				t.Errorf("%s 的温度源 %q 没配置小数位寄存器（会读到寄存器 0x000）",
					name, ts.name)
			}
			if ts.halfBit > 7 {
				t.Errorf("%s 的温度源 %q 的小数位编号 %d 越界（应为 0-7）",
					name, ts.name, ts.halfBit)
			}
		}
	}

	// NCT668x 的小数位固定在 reg+1 的第 7 位。
	for _, ts := range layoutNCT668x.tempSources {
		if ts.halfReg != ts.reg+1 {
			t.Errorf("NCT668x 温度源 %q 的小数位寄存器应为 0x%03X，得到 0x%03X",
				ts.name, ts.reg+1, ts.halfReg)
		}
		if ts.halfBit != 7 {
			t.Errorf("NCT668x 温度源 %q 的小数位编号应为 7，得到 %d", ts.name, ts.halfBit)
		}
		if ts.halfLSB {
			t.Errorf("NCT668x 温度源 %q 的整数部分以 1 度为单位，不该用 halfLSB", ts.name)
		}
	}

	// NCT679x 的整数部分以 2 度为单位，低位才是小数位。
	for _, ts := range layoutNCT679x.tempSources {
		if ts.halfBit != noHalfBit && !ts.halfLSB {
			t.Errorf("NCT679x 温度源 %q 应当使用 halfLSB（整数部分以 2 度为单位）", ts.name)
		}
	}
}
