package main

// 芯片识别与寄存器换算的纯逻辑部分。
//
// 这些函数与平台无关，单独放在这里便于单元测试：
// 芯片表一旦写错，表现就是"探测不到芯片"，很难在现场排查。

// 13 位风扇计数器的换算常量，与 LibreHardwareMonitor 保持一致。
const (
	maxFanCount = 0x1FFF
	minFanCount = 0x15
)

// chipFamily 表示芯片所属的寄存器布局家族。
type chipFamily int

const (
	// familyUnknown 表示已识别芯片但本程序尚未实现其寄存器表。
	familyUnknown chipFamily = iota
	// familyNCT668x 是 Nuvoton NCT6683D / 6686D / 6687D（EC 风格访问）。
	familyNCT668x
	// familyNCT679x 是 Nuvoton NCT679x 系列（bank 选择风格访问）。
	familyNCT679x
)

// chipInfo 描述一块 Super I/O 芯片。
type chipInfo struct {
	name   string
	family chipFamily
}

// lookupChip 根据芯片 ID 与修订号识别型号。
//
// 判定规则来自 LibreHardwareMonitor v0.9.6 的 Hardware/Motherboard/Lpc/LpcIO.cs，
// 逐条对齐（包括部分型号用修订号高位掩码匹配的做法）。
func lookupChip(id, rev byte) (chipInfo, bool) {
	switch id {
	case 0xB4:
		if rev&0xF0 == 0x70 {
			return chipInfo{"Nuvoton NCT6771F", familyNCT679x}, true
		}
	case 0xC3:
		if rev&0xF0 == 0x30 {
			return chipInfo{"Nuvoton NCT6776F", familyNCT679x}, true
		}
	case 0xC4:
		if rev&0xF0 == 0x50 {
			return chipInfo{"Nuvoton NCT610XD", familyNCT679x}, true
		}
	case 0xC5:
		if rev&0xF0 == 0x60 {
			return chipInfo{"Nuvoton NCT6779D", familyNCT679x}, true
		}
	case 0xC7:
		if rev == 0x32 {
			return chipInfo{"Nuvoton NCT6683D", familyNCT668x}, true
		}
	case 0xC8:
		if rev == 0x03 {
			return chipInfo{"Nuvoton NCT6791D", familyNCT679x}, true
		}
	case 0xC9:
		switch rev {
		case 0x11:
			return chipInfo{"Nuvoton NCT6792D", familyNCT679x}, true
		case 0x13:
			return chipInfo{"Nuvoton NCT6792DA", familyNCT679x}, true
		}
	case 0xD1:
		if rev == 0x21 {
			return chipInfo{"Nuvoton NCT6793D", familyNCT679x}, true
		}
	case 0xD3:
		if rev == 0x52 {
			return chipInfo{"Nuvoton NCT6795D", familyNCT679x}, true
		}
	case 0xD4:
		switch rev {
		case 0x23:
			return chipInfo{"Nuvoton NCT6796D", familyNCT679x}, true
		case 0x2A:
			return chipInfo{"Nuvoton NCT6796DR", familyNCT679x}, true
		case 0x2B:
			return chipInfo{"Nuvoton NCT6798D", familyNCT679x}, true
		case 0x40, 0x41:
			// NCT6686D：联想 ThinkCentre M720q 用的是这一款。
			return chipInfo{"Nuvoton NCT6686D", familyNCT668x}, true
		case 0x51:
			return chipInfo{"Nuvoton NCT6797D", familyNCT679x}, true
		}
	case 0xD5:
		if rev == 0x92 {
			return chipInfo{"Nuvoton NCT6687D", familyNCT668x}, true
		}
	case 0xD8:
		switch rev {
		case 0x02:
			return chipInfo{"Nuvoton NCT6799D", familyNCT679x}, true
		case 0x06:
			return chipInfo{"Nuvoton NCT6701D", familyNCT679x}, true
		}

	// Winbond 系列。本程序暂不实现其寄存器表，只做识别与提示。
	case 0x17, 0x3A, 0x41:
		return chipInfo{"Winbond W83627HF", familyUnknown}, true
	case 0x82:
		if rev&0xF0 == 0x80 {
			return chipInfo{"Winbond W83627THF", familyUnknown}, true
		}
	case 0x85:
		if rev == 0x41 {
			return chipInfo{"Winbond W83687THF", familyUnknown}, true
		}
	case 0x88:
		if rev&0xF0 == 0x50 || rev&0xF0 == 0x60 {
			return chipInfo{"Winbond W83627EHF", familyUnknown}, true
		}
	case 0xA0:
		if rev&0xF0 == 0x20 {
			return chipInfo{"Winbond W83627DHG", familyUnknown}, true
		}
	case 0xA5:
		if rev&0xF0 == 0x10 {
			return chipInfo{"Winbond W83667HG", familyUnknown}, true
		}
	case 0xB0:
		if rev&0xF0 == 0x70 {
			return chipInfo{"Winbond W83627DHGP", familyUnknown}, true
		}
	case 0xB3:
		if rev&0xF0 == 0x50 {
			return chipInfo{"Winbond W83667HGB", familyUnknown}, true
		}
	}
	return chipInfo{}, false
}

// lookupIT87Chip 识别 ITE IT87xx 系列。
//
// IT87 的芯片 ID 是 16 位的（0x20 存高位、0x21 存低位），
// 与 Nuvoton 的"0x20 存 ID、0x21 存修订"约定不同。
func lookupIT87Chip(id, rev byte) (chipInfo, bool) {
	word := uint16(id)<<8 | uint16(rev)
	switch word {
	case 0x8613:
		return chipInfo{"ITE IT8613E", familyUnknown}, true
	case 0x8620:
		return chipInfo{"ITE IT8620E", familyUnknown}, true
	case 0x8625:
		return chipInfo{"ITE IT8625E", familyUnknown}, true
	case 0x8628:
		return chipInfo{"ITE IT8628E", familyUnknown}, true
	case 0x8631:
		return chipInfo{"ITE IT8631E", familyUnknown}, true
	case 0x8638:
		return chipInfo{"ITE IT8638E", familyUnknown}, true
	case 0x8655:
		return chipInfo{"ITE IT8655E", familyUnknown}, true
	case 0x8665:
		return chipInfo{"ITE IT8665E", familyUnknown}, true
	case 0x8686:
		return chipInfo{"ITE IT8686E", familyUnknown}, true
	case 0x8688:
		return chipInfo{"ITE IT8688E", familyUnknown}, true
	case 0x8689:
		return chipInfo{"ITE IT8689E", familyUnknown}, true
	case 0x8695:
		return chipInfo{"ITE IT87952E", familyUnknown}, true
	case 0x8696:
		return chipInfo{"ITE IT8696E", familyUnknown}, true
	case 0x8705:
		return chipInfo{"ITE IT8705F", familyUnknown}, true
	case 0x8712:
		return chipInfo{"ITE IT8712F", familyUnknown}, true
	case 0x8716:
		return chipInfo{"ITE IT8716F", familyUnknown}, true
	case 0x8718:
		return chipInfo{"ITE IT8718F", familyUnknown}, true
	case 0x8720:
		return chipInfo{"ITE IT8720F", familyUnknown}, true
	case 0x8721:
		return chipInfo{"ITE IT8721F", familyUnknown}, true
	case 0x8726:
		return chipInfo{"ITE IT8726F", familyUnknown}, true
	case 0x8728:
		return chipInfo{"ITE IT8728F", familyUnknown}, true
	case 0x8733:
		return chipInfo{"ITE IT8792E", familyUnknown}, true
	case 0x8771:
		return chipInfo{"ITE IT8771E", familyUnknown}, true
	case 0x8772:
		return chipInfo{"ITE IT8772E", familyUnknown}, true
	case 0x8790:
		return chipInfo{"ITE IT8790E", familyUnknown}, true
	}
	return chipInfo{}, false
}

// normalizeBaseAddress 按 LibreHardwareMonitor 的做法修正基址。
//
// 有些 Fintek 芯片的基址寄存器里已经带上了 0x05 的地址寄存器偏移。
func normalizeBaseAddress(addr uint16) uint16 {
	if addr&0x07 == 0x05 {
		return addr & 0xFFF8
	}
	return addr
}

// baseFromSuperIOWord 从 superio_inw 的结果算出硬件监控的运行时基址。
//
// 这里必须与 PawnIO 模块的 find_bars 完全一致：模块用 superio_inw(0x60)
// 读基址，规范化后放进驱动内部的端口白名单，而驱动只放行白名单里的端口。
// superio_inw 的字节序是"高位在前"：(byte@0x60 << 8) | byte@0x61。
//
// 实机验证：NEC8（M720q，NCT6686D，配置端口 0x2E，芯片 ID 0xD4 修订 0x41）
// 在硬件监控逻辑设备 0x0B 上，基址寄存器 0x60 读到 0x0A、0x61 读到 0x20，
// 因此运行时基址是 0x0A20。
func baseFromSuperIOWord(word uint16) uint16 {
	return normalizeBaseAddress(word)
}

// baseAddressValid 校验基址是否合理。
func baseAddressValid(addr uint16) bool {
	return addr >= 0x100 && addr&0xF007 == 0
}

// rpmFrom13Bit 按 13 位计数器换算转速。
func rpmFrom13Bit(high, low byte) float64 {
	count := int(high)<<5 | int(low&0x1F)
	if count >= maxFanCount {
		return 0
	}
	if count < minFanCount {
		return 0
	}
	return 1.35e6 / float64(count)
}

// containsU16 判断切片里是否包含某个值。
func containsU16(list []uint16, v uint16) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// noHalfBit 表示这个温度源没有 0.5 度的小数位。
//
// LibreHardwareMonitor 里对应的写法是 TemperatureSourceData 的 halfBit 默认值 -1。
const noHalfBit = 0xFF

// tempSource 描述一个温度传感器。
type tempSource struct {
	name string
	// reg 是整数部分的寄存器地址。
	reg uint16
	// halfReg 是小数位所在寄存器；halfBit == noHalfBit 表示没有小数位。
	halfReg uint16
	halfBit uint8
	// halfLSB 为真时小数位是 0.5 度的整数部分（LSB 为 1 度）。
	// Nuvoton NCT679x 属于这一类；NCT668x 的整数部分本身就是 1 度。
	halfLSB bool
}

// tempWithHalf 构造一个带 0.5 度小数位的温度源。
//
// LibreHardwareMonitor 的 NCT668x 分支是硬编码的：
//
//	value = (sbyte)ReadByte(reg);
//	half  = (ReadByte(reg + 1) >> 7) & 0x1;
//	temperature = value + 0.5f * half;
//
// 也就是说小数位固定在下一个寄存器的第 7 位，整数部分以 1 度为单位。
func tempWithHalf(name string, reg uint16) tempSource {
	return tempSource{name: name, reg: reg, halfReg: reg + 1, halfBit: 7}
}

// superIOLayout 描述一类芯片的寄存器布局。
type superIOLayout struct {
	kind     string
	channels int
	// tempSources 是温度传感器列表。
	tempSources []tempSource
	// rpmRegs 是风扇转速寄存器（每个通道占 2 字节，高字节在前）。
	rpmRegs []uint16
	// countRegs 是 13 位转速计数器寄存器，命中时走换算公式。
	countRegs []uint16
	// dutyRegs 是占空比读取寄存器。
	dutyRegs []uint16
	// pwmRegs 是占空比写入寄存器。
	pwmRegs []uint16
	// modeRegs 是"进入手动模式"的寄存器。
	modeRegs []uint16
	// requestReg 是 NCT668x 的访问请求握手寄存器，0 表示不需要。
	requestReg uint16
	// ecSpace 为真表示运行时寄存器走 EC 风格访问（页/索引/数据）。
	ecSpace bool
}

// layoutNCT668x 是 NCT6683D / NCT6686D / NCT6687D 的寄存器布局。
//
// 联想 ThinkCentre M720q 用的是 NCT6686D，正是这一族。
var layoutNCT668x = superIOLayout{
	kind:     "nct668x",
	channels: 8,
	tempSources: []tempSource{
		tempWithHalf("CPU", 0x100),
		tempWithHalf("System", 0x102),
		tempWithHalf("MOS", 0x104),
		tempWithHalf("PCH", 0x106),
		tempWithHalf("CPU Socket", 0x108),
		tempWithHalf("PCIE_1", 0x10A),
		tempWithHalf("M2_1", 0x10C),
		tempWithHalf("PCIE_2", 0x10E),
		tempWithHalf("M2_2", 0x110),
		tempWithHalf("PCIE_3", 0x112),
		tempWithHalf("M2_4", 0x114),
	},
	rpmRegs: []uint16{
		0x140, 0x142, 0x144, 0x146, 0x148, 0x14A, 0x14C, 0x14E,
		0x150, 0x152, 0x154, 0x156, 0x158, 0x15A, 0x15C, 0x15E,
	},
	countRegs:  nil,
	dutyRegs:   []uint16{0x160, 0x161, 0x162, 0x163, 0x164, 0x165, 0x166, 0x167},
	pwmRegs:    []uint16{0xA28, 0xA29, 0xA2A, 0xA2B, 0xA2C, 0xA2D, 0xA2E, 0xA2F},
	modeRegs:   []uint16{0xA00, 0xA00, 0xA00, 0xA00, 0xA00, 0xA00, 0xA00, 0xA00},
	requestReg: 0xA01,
	ecSpace:    true,
}

// layoutNCT679x 是 NCT679x 家族的寄存器布局。
var layoutNCT679x = superIOLayout{
	kind:     "nct679x",
	channels: 7,
	rpmRegs: []uint16{
		0x4B0, 0x4B2, 0x4B4, 0x4B6, 0x4B8, 0x4BA, 0x4CC,
	},
	countRegs: []uint16{0x4B0, 0x4B2, 0x4B4, 0x4B6, 0x4B8, 0x4BA, 0x4CC},
	dutyRegs:  []uint16{0x001, 0x003, 0x011, 0x013, 0x015, 0xA09, 0xB09},
	pwmRegs:   []uint16{0x109, 0x209, 0x309, 0x809, 0x909, 0xA09, 0xB09},
	modeRegs:  []uint16{0x102, 0x202, 0x302, 0x802, 0x902, 0xA02, 0xB02},
}

// layoutNCT679xAlt 是 NCT6796D / NCT6797D 一类的温度寄存器排布。
var layoutNCT679xAlt = superIOLayout{
	kind:     "nct679x",
	channels: 7,
	tempSources: []tempSource{
		{name: "CPU", reg: 0x073, halfReg: 0x074, halfBit: 7, halfLSB: true},
		{name: "System", reg: 0x075, halfReg: 0x076, halfBit: 7, halfLSB: true},
		{name: "AUXTIN0", reg: 0x077, halfReg: 0x078, halfBit: 7, halfLSB: true},
		{name: "AUXTIN1", reg: 0x079, halfReg: 0x07A, halfBit: 7, halfLSB: true},
		{name: "AUXTIN2", reg: 0x07B, halfReg: 0x07C, halfBit: 7, halfLSB: true},
		{name: "AUXTIN3", reg: 0x07D, halfReg: 0x07E, halfBit: 7, halfLSB: true},
	},
	rpmRegs: []uint16{
		0x4B0, 0x4B2, 0x4B4, 0x4B6, 0x4B8, 0x4BA, 0x4CC,
	},
	countRegs: []uint16{0x4B0, 0x4B2, 0x4B4, 0x4B6, 0x4B8, 0x4BA, 0x4CC},
	dutyRegs:  []uint16{0x001, 0x003, 0x011, 0x013, 0x015, 0x017, 0x029},
	pwmRegs:   []uint16{0x109, 0x209, 0x309, 0x809, 0x909, 0xA09, 0xB09},
	modeRegs:  []uint16{0x102, 0x202, 0x302, 0x802, 0x902, 0xA02, 0xB02},
}

// layoutNCT679xD 是 NCT6798D / NCT6799D 一类的温度寄存器排布。
var layoutNCT679xD = superIOLayout{
	kind:     "nct679x",
	channels: 7,
	tempSources: []tempSource{
		{name: "CPU", reg: 0x075, halfReg: 0x076, halfBit: 7, halfLSB: true},
		{name: "System", reg: 0x077, halfReg: 0x078, halfBit: 7, halfLSB: true},
		{name: "AUXTIN0", reg: 0x079, halfReg: 0x07A, halfBit: 7, halfLSB: true},
		{name: "AUXTIN1", reg: 0x07B, halfReg: 0x07C, halfBit: 7, halfLSB: true},
		{name: "AUXTIN2", reg: 0x07D, halfReg: 0x07E, halfBit: 7, halfLSB: true},
		{name: "AUXTIN3", reg: 0x4A0, halfReg: 0x49E, halfBit: 6, halfLSB: true},
		// AUXTIN4 没有 0.5 度小数位（LibreHardwareMonitor 里 halfBit = -1）。
		{name: "AUXTIN4", reg: 0x027, halfBit: noHalfBit},
	},
	rpmRegs: []uint16{
		0x4B0, 0x4B2, 0x4B4, 0x4B6, 0x4B8, 0x4BA, 0x4CC,
	},
	countRegs: []uint16{0x4B0, 0x4B2, 0x4B4, 0x4B6, 0x4B8, 0x4BA, 0x4CC},
	dutyRegs:  []uint16{0x001, 0x003, 0x011, 0x013, 0x015, 0xA09, 0xB09},
	pwmRegs:   []uint16{0x109, 0x209, 0x309, 0x809, 0x909, 0xA09, 0xB09},
	modeRegs:  []uint16{0x102, 0x202, 0x302, 0x802, 0x902, 0xA02, 0xB02},
}

// layoutFor 根据芯片型号选取寄存器布局。
func layoutFor(info chipInfo) superIOLayout {
	switch info.family {
	case familyNCT668x:
		return layoutNCT668x
	case familyNCT679x:
		switch info.name {
		case "Nuvoton NCT6798D", "Nuvoton NCT6799D":
			return layoutNCT679xD
		case "Nuvoton NCT6796D", "Nuvoton NCT6796DR", "Nuvoton NCT6797D":
			return layoutNCT679xAlt
		default:
			return layoutNCT679x
		}
	}
	return superIOLayout{kind: "unsupported"}
}

// nct668xFanNames 是 NCT668x 风扇通道的常用名称。
// 联想 M720q 上通常是：CPU 风扇、水冷/泵、机箱风扇……
var nct668xFanNames = []string{
	"CPU 风扇", "PUMP 风扇", "机箱风扇 1", "机箱风扇 2",
	"机箱风扇 3", "机箱风扇 4", "机箱风扇 5", "机箱风扇 6",
}

// nct679xFanNames 是 NCT679x 风扇通道的常用名称。
var nct679xFanNames = []string{
	"CPU 风扇", "PUMP 风扇", "机箱风扇 1", "机箱风扇 2",
	"机箱风扇 3", "机箱风扇 4", "机箱风扇 5",
}
