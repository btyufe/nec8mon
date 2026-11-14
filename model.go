package main

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Reading 是一条归一化后的传感器读数。
// 所有数据源（LHM HTTP / ACPI / nvidia-smi）都会转换成本结构，
// 上层（HTTP API + 前端）只关心这一种形态。
type Reading struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Category string  `json:"category"` // Temperature / Fan / Load / Clock / Power / Voltage
	Hardware string  `json:"hardware"` // 所属硬件，如 "Intel Core i5-8500T"
	Value    float64 `json:"value"`
	Unit     string  `json:"unit"`
	Min      float64 `json:"min"`
	Max      float64 `json:"max"`
	Source   string  `json:"source"`
}

// hwTemp 是一条直接从硬件芯片读到的温度。
type hwTemp struct {
	Name  string
	Value float64
}

// hwFan 是一条直接从硬件芯片读到的风扇状态。
type hwFan struct {
	Name string
	RPM  float64
	Duty float64
	// HasDuty 为真表示该通道有可读的占空比寄存器（也就是可调速通道）。
	HasDuty bool
	// Channel 是调速通道号（0 起）；只读测速口为 -1。
	Channel int
	// Index 是该风扇在转速寄存器表里的下标，用来区分只读测速口。
	Index int
}

// SourceStatus 描述单个数据源当前的健康状况。
type SourceStatus struct {
	Name    string    `json:"name"`
	OK      bool      `json:"ok"`
	Detail  string    `json:"detail,omitempty"`
	Count   int       `json:"count"`
	Updated time.Time `json:"updatedAt"`
}

// Snapshot 是一次采集的完整快照，也是 /api/sensors 的返回体。
type Snapshot struct {
	Hostname       string         `json:"hostname"`
	UpdatedAt      time.Time      `json:"updatedAt"`
	UptimeSeconds  float64        `json:"uptimeSeconds"`
	Sources        []SourceStatus `json:"sources"`
	Warnings       []string       `json:"warnings"`
	Readings       []Reading      `json:"readings"`
	CPUTemperature *Reading       `json:"cpuTemperature"`
	Fan            *Reading       `json:"fan"`
	FanPercent     *Reading       `json:"fanPercent"`
}

// historyPoint 用于前端画迷你趋势图。
type historyPoint struct {
	T     time.Time `json:"t"`
	Temp  *float64  `json:"temp"`
	Fan   *float64  `json:"fan"`
	Temp2 *float64  `json:"temp2"`
}

// 各类传感器的类别常量。
const (
	catTemperature = "Temperature"
	catFan         = "Fan"
	catLoad        = "Load"
	catClock       = "Clock"
	catPower       = "Power"
	catVoltage     = "Voltage"
	catCurrent     = "Current"
)

// unitForCategory 返回该类别传感器的默认单位。
func unitForCategory(category string) string {
	switch category {
	case catTemperature:
		return "°C"
	case catFan:
		return "RPM"
	case catLoad:
		return "%"
	case catClock:
		return "MHz"
	case catPower:
		return "W"
	case catVoltage:
		return "V"
	case catCurrent:
		return "A"
	default:
		return ""
	}
}

// valueRE 用于从 "45.6 °C" / "1,200 RPM" 这类带单位的字符串里抠出数字。
var valueRE = regexp.MustCompile(`^\s*([-+]?[0-9]*\.?[0-9]+(?:[eE][-+]?[0-9]+)?)\s*(.*)$`)

// parseValue 解析 LHM 格式化后的数值字符串。
// 返回数值、单位以及是否解析成功。
func parseValue(s string) (float64, string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, "", false
	}
	m := valueRE.FindStringSubmatch(s)
	if m == nil {
		return 0, "", false
	}
	var v float64
	if _, err := fmt.Sscanf(m[1], "%g", &v); err != nil {
		return 0, "", false
	}
	unit := strings.TrimSpace(m[2])
	unit = strings.Trim(unit, "()[]")
	return v, unit, true
}

// pickPrimaryCPU 从读数集合里挑出最能代表"CPU 温度"的一条。
// 优先级：CPU Package > Package/Tctl/Tdie > Core Average > Core Max > 其它 CPU 温度。
func pickPrimaryCPU(readings []Reading) *Reading {
	best := -1
	bestScore := -1
	for i := range readings {
		r := readings[i]
		if r.Category != catTemperature {
			continue
		}
		if !looksLikeCPU(r) {
			continue
		}
		score := cpuNameScore(r.Name)
		if score > bestScore {
			bestScore, best = score, i
		}
	}
	if best < 0 {
		return nil
	}
	return &readings[best]
}

// looksLikeCPU 判断一条读数是否来自 CPU。
func looksLikeCPU(r Reading) bool {
	if isGPUish(r) {
		return false
	}
	id := strings.ToLower(r.ID)
	if strings.Contains(id, "cpu") {
		return true
	}
	hw := strings.ToLower(r.Hardware)
	if strings.Contains(hw, "cpu") {
		return true
	}
	// 形如 "Intel Core i5-8500T" 的处理器名称
	if strings.Contains(hw, "core") && (strings.Contains(hw, "intel") || strings.Contains(hw, "ryzen") || strings.Contains(hw, "core i")) {
		return true
	}
	return cpuNameScore(r.Name) > 0
}

// cpuNameScore 给 CPU 温度传感器名称打分，分数越高越优先。
func cpuNameScore(name string) int {
	n := strings.ToLower(strings.TrimSpace(name))
	switch {
	case n == "cpu package", n == "package", n == "tctl/tdie", n == "tdie":
		return 100
	case n == "core average", n == "average":
		return 80
	case n == "core max", n == "max", n == "maximum":
		return 70
	case strings.Contains(n, "package"):
		return 90
	case strings.HasPrefix(n, "core"):
		return 50
	default:
		return 0
	}
}

// pickPrimaryFan 从读数集合里挑出主风扇（单位 RPM）。
// 可选传入 preferredChannel（>= 0），优先匹配当前控制通道；未传入或未找到时按转速与名称规则挑选。
//
// 挑选顺序：
//  1. 若指定了 preferredChannel，优先返回该通道的风扇（实现主风扇与控制目标联动）。
//  2. 正在转的通道优先。现场实测遇到过"名字叫 CPU 风扇但读数恒为 0"的
//     主板——那个位置根本没接风扇，真正在转的是另一个通道。若只按名字挑，
//     界面就会一直显示 0 RPM，看起来像读数坏了。
//  3. 同样在转时，可控通道优先于只读测速口，名字像 CPU 风扇的再优先。
//  4. 全部停转时退回按名字打分，至少能把"哪个通道没在转"显示出来。
func pickPrimaryFan(readings []Reading, preferredChannel ...int) *Reading {
	if len(preferredChannel) > 0 && preferredChannel[0] >= 0 {
		target := preferredChannel[0]
		for i := range readings {
			r := readings[i]
			if r.Category != catFan || !strings.EqualFold(r.Unit, "RPM") {
				continue
			}
			if isGPUish(r) {
				continue
			}
			if ch, ok := channelFromFanID(r.ID); ok && ch == target {
				return &readings[i]
			}
		}
	}

	best := -1
	for i := range readings {
		r := readings[i]
		if r.Category != catFan || !strings.EqualFold(r.Unit, "RPM") {
			continue
		}
		if isGPUish(r) {
			continue
		}
		if best < 0 || fanBetter(r, readings[best]) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	return &readings[best]
}

// fanBetter 判断候选风扇 a 是否比当前选中的 b 更适合当作主风扇。
func fanBetter(a, b Reading) bool {
	// 1. 正在转的优先
	aActive, bActive := a.Value > 0, b.Value > 0
	if aActive != bActive {
		return aActive
	}
	// 2. 可控通道优先（只读测速口没有对应的 PWM 寄存器）
	aCtl, bCtl := fanControllable(a), fanControllable(b)
	if aCtl != bCtl {
		return aCtl
	}
	// 3. 名字像 CPU 风扇的优先
	as, bs := fanNameScore(a), fanNameScore(b)
	if as != bs {
		return as > bs
	}
	// 4. 转速高的优先
	return a.Value > b.Value
}

// fanControllable 判断这条风扇读数是否来自一个可控（可调速）的通道。
func fanControllable(r Reading) bool {
	return strings.Contains(strings.ToLower(r.ID), "/fan/")
}

// fanNameScore 给风扇通道名称打分，分数越高越像"CPU 风扇"。
func fanNameScore(r Reading) int {
	id := strings.ToLower(r.ID)
	hw := strings.ToLower(r.Hardware)
	name := strings.ToLower(r.Name)
	switch {
	case strings.Contains(name, "cpu") || strings.Contains(id, "cpu") || strings.Contains(hw, "cpu"):
		return 100
	case strings.Contains(name, "fan") || strings.Contains(id, "fan") || strings.Contains(hw, "fan"):
		return 60
	}
	return 10
}

// isGPUish 判断读数是否来自显卡（显卡风扇不作为"主机风扇"）。
func isGPUish(r Reading) bool {
	id := strings.ToLower(r.ID)
	hw := strings.ToLower(r.Hardware)
	name := strings.ToLower(r.Name)
	for _, kw := range []string{"gpu", "nvidia", "geforce", "radeon", "amd ", "intel hd", "arc "} {
		if strings.Contains(id, kw) || strings.Contains(hw, kw) || strings.Contains(name, kw) {
			return true
		}
	}
	return false
}

// fanPercentReading 找一个风扇调速百分比（0-100），作为转速的补充参考。
// LHM 把这类传感器归为 Control（控制量）而非 Fan，因此两种类别都要认。
//
// 优先取与主风扇"同一个通道"的占空比：superio/fan/1 对应 superio/control/1。
// 同通道匹配不上时，退回按硬件归属匹配，避免拿到显卡风扇的百分比。
func fanPercentReading(readings []Reading, primaryFan *Reading) *Reading {
	match := func(r Reading) bool {
		if r.Unit != "%" {
			return false
		}
		return r.Category == catFan || r.Category == "Control" || strings.Contains(strings.ToLower(r.Name), "fan")
	}

	if primaryFan != nil {
		if ch, ok := channelFromFanID(primaryFan.ID); ok {
			for i := range readings {
				r := readings[i]
				if !match(r) || r.Hardware != primaryFan.Hardware {
					continue
				}
				if c, ok := channelFromFanID(r.ID); ok && c == ch {
					return &readings[i]
				}
			}
		}
		for i := range readings {
			r := readings[i]
			if match(r) && r.Hardware == primaryFan.Hardware {
				return &readings[i]
			}
		}
	}
	for i := range readings {
		if match(readings[i]) {
			return &readings[i]
		}
	}
	return nil
}

// formatValue 把数值按单位格式化，便于日志输出。
func formatValue(v float64, unit string) string {
	switch unit {
	case "°C", "%", "W", "V", "A", "MHz":
		return fmt.Sprintf("%.1f %s", v, unit)
	case "RPM":
		return fmt.Sprintf("%.0f %s", v, unit)
	default:
		if v == math.Trunc(v) {
			return fmt.Sprintf("%.0f %s", v, unit)
		}
		return fmt.Sprintf("%.2f %s", v, unit)
	}
}

// displayWidth 计算字符串在等宽终端里占多少列（中文与全角字符按 2 列算）。
//
// Go 的 %-30s 是按字节数补空格的，中文一个字符 3 字节，表格会错位，
// 所以这里自己算一遍显示宽度。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115F, // 韩文字母
			r >= 0x2E80 && r <= 0xA4CF, // CJK 部首、扩展 A、汉字
			r >= 0xAC00 && r <= 0xD7A3, // 韩文音节
			r >= 0xF900 && r <= 0xFAFF, // CJK 兼容表意文字
			r >= 0xFE30 && r <= 0xFE6F, // CJK 兼容形式
			r >= 0xFF00 && r <= 0xFF60, // 全角 ASCII
			r >= 0xFFE0 && r <= 0xFFE6: // 全角符号
			w += 2
		default:
			w++
		}
	}
	return w
}

// padRight 按显示宽度在右侧补空格，用于对齐表格。
func padRight(s string, width int) string {
	w := displayWidth(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}
