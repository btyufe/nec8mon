package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// loadSample 读取模拟的 LibreHardwareMonitor data.json。
func loadSample(t *testing.T) []Reading {
	t.Helper()
	raw, err := os.ReadFile("testdata/sample_data.json")
	if err != nil {
		t.Fatalf("读取样例失败: %v", err)
	}
	var root lhmNode
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("解析样例失败: %v", err)
	}
	var out []Reading
	walkLHM(&root, "", &out)
	return out
}

func TestWalkLHM(t *testing.T) {
	readings := loadSample(t)
	if len(readings) != 15 {
		t.Fatalf("期望 15 条读数，实际 %d 条", len(readings))
	}

	// 硬件归属要正确继承
	byID := map[string]Reading{}
	for _, r := range readings {
		byID[r.ID] = r
	}

	cases := []struct {
		id       string
		hardware string
		value    float64
		unit     string
	}{
		{"/intelcpu/0/temperature/0", "Intel Core i5-8500T", 45.6, "°C"},
		{"/lpc/nct6798/fan/0", "Nuvoton NCT6798", 1180, "RPM"},
		{"/lpc/nct6798/temperature/0", "Nuvoton NCT6798", 33.0, "°C"},
		{"/lpc/nct6798/voltage/0", "Nuvoton NCT6798", 1.176, "V"},
		{"/gpu-nvidia/0/temperature/0", "GeForce GTX 1650", 39.0, "°C"},
		{"/intelcpu/0/load/0", "Intel Core i5-8500T", 18.3, "%"},
	}
	for _, c := range cases {
		r, ok := byID[c.id]
		if !ok {
			t.Errorf("缺少读数 %s", c.id)
			continue
		}
		if r.Hardware != c.hardware {
			t.Errorf("%s 硬件归属 = %q，期望 %q", c.id, r.Hardware, c.hardware)
		}
		if diff := r.Value - c.value; diff > 0.001 || diff < -0.001 {
			t.Errorf("%s 数值 = %v，期望 %v", c.id, r.Value, c.value)
		}
		if r.Unit != c.unit {
			t.Errorf("%s 单位 = %q，期望 %q", c.id, r.Unit, c.unit)
		}
	}
}

func TestPickPrimaryCPU(t *testing.T) {
	readings := loadSample(t)
	cpu := pickPrimaryCPU(readings)
	if cpu == nil {
		t.Fatal("没有挑出 CPU 温度")
	}
	if cpu.ID != "/intelcpu/0/temperature/0" {
		t.Errorf("CPU 温度选错 = %s (%s)，期望 /intelcpu/0/temperature/0", cpu.ID, cpu.Name)
	}
	if cpu.Value != 45.6 {
		t.Errorf("CPU 温度 = %v，期望 45.6", cpu.Value)
	}
}

func TestPickPrimaryFan(t *testing.T) {
	readings := loadSample(t)
	fan := pickPrimaryFan(readings)
	if fan == nil {
		t.Fatal("没有挑出风扇")
	}
	// 应选中板载 "CPU Fan"（1180 RPM），而不是 SYS Fan(0) 或显卡风扇
	if fan.ID != "/lpc/nct6798/fan/0" {
		t.Errorf("风扇选错 = %s (%s)，期望 /lpc/nct6798/fan/0", fan.ID, fan.Name)
	}
	if fan.Value != 1180 {
		t.Errorf("风扇转速 = %v，期望 1180", fan.Value)
	}
	if fan.Unit != "RPM" {
		t.Errorf("风扇单位 = %q，期望 RPM", fan.Unit)
	}
}

// TestPickPrimaryFanPrefersRunning 复现现场故障：
// "CPU 风扇"那个位置没接风扇（恒为 0 RPM），真正在转的是另一个通道。
// 只按名字打分的话界面会一直显示 0。
func TestPickPrimaryFanPrefersRunning(t *testing.T) {
	readings := []Reading{
		{ID: "superio/fan/0", Name: "通道 0 · CPU 风扇", Category: catFan, Unit: "RPM", Value: 0},
		{ID: "superio/fan/1", Name: "通道 1 · PUMP 风扇", Category: catFan, Unit: "RPM", Value: 1015},
		{ID: "superio/fan/2", Name: "通道 2 · 机箱风扇 1", Category: catFan, Unit: "RPM", Value: 0},
	}
	fan := pickPrimaryFan(readings)
	if fan == nil {
		t.Fatal("没有挑出风扇")
	}
	if fan.ID != "superio/fan/1" {
		t.Errorf("风扇选错 = %s (%s)，期望在转的 superio/fan/1", fan.ID, fan.Name)
	}
	if fan.Value != 1015 {
		t.Errorf("风扇转速 = %v，期望 1015", fan.Value)
	}
}

// TestPickPrimaryFanAllStopped 全部停转时仍然要挑一个出来（显示 0 而不是"无数据"）。
func TestPickPrimaryFanAllStopped(t *testing.T) {
	readings := []Reading{
		{ID: "superio/fan/0", Name: "通道 0 · CPU 风扇", Category: catFan, Unit: "RPM", Value: 0},
		{ID: "superio/fan/1", Name: "通道 1 · PUMP 风扇", Category: catFan, Unit: "RPM", Value: 0},
	}
	fan := pickPrimaryFan(readings)
	if fan == nil {
		t.Fatal("全部停转时也应当挑出一个通道")
	}
	if fan.ID != "superio/fan/0" {
		t.Errorf("风扇选错 = %s，期望 superio/fan/0", fan.ID)
	}
}

// TestSuperIORereplayNEC8 用 NEC8 实机诊断报告里的真实数据，
// 复现"网页风扇转速一直为 0"这个故障，并确认修复后整条链路都正确。
//
// 实机数据（v2.2.0 诊断）：通道 0 名字叫"CPU 风扇"但恒为 0 RPM，
// 通道 1（PUMP）才是真正在转的那个：1015 RPM / 39%。
func TestSuperIORereplayNEC8(t *testing.T) {
	readings := []Reading{
		{ID: "superio/temp/CPU", Name: "CPU", Category: catTemperature, Unit: "°C", Value: 34.5,
			Hardware: "Nuvoton NCT6686D", Source: "superio"},
		{ID: "superio/fan/0", Name: "通道 0 · CPU 风扇", Category: catFan, Unit: "RPM", Value: 0,
			Hardware: "Nuvoton NCT6686D", Source: "superio"},
		{ID: "superio/control/0", Name: "通道 0 · CPU 风扇 占空比", Category: "Control", Unit: "%", Value: 0,
			Hardware: "Nuvoton NCT6686D", Source: "superio"},
		{ID: "superio/fan/1", Name: "通道 1 · PUMP 风扇", Category: catFan, Unit: "RPM", Value: 1015,
			Hardware: "Nuvoton NCT6686D", Source: "superio"},
		{ID: "superio/control/1", Name: "通道 1 · PUMP 风扇 占空比", Category: "Control", Unit: "%", Value: 39,
			Hardware: "Nuvoton NCT6686D", Source: "superio"},
		{ID: "superio/fan/2", Name: "通道 2 · 机箱风扇 1", Category: catFan, Unit: "RPM", Value: 0,
			Hardware: "Nuvoton NCT6686D", Source: "superio"},
		{ID: "superio/tach/8", Name: "附加测速 1（只读）", Category: catFan, Unit: "RPM", Value: 0,
			Hardware: "Nuvoton NCT6686D", Source: "superio"},
	}

	// 1) 主风扇必须是正在转的通道 1，而不是名字最像 CPU 的通道 0
	primary := pickPrimaryFan(readings)
	if primary == nil || primary.ID != "superio/fan/1" {
		t.Fatalf("主风扇选错: %+v", primary)
	}
	if primary.Value != 1015 {
		t.Errorf("主风扇转速 = %v，期望 1015", primary.Value)
	}

	// 2) 占空比必须取同通道的 39%，不是通道 0 的 0%
	pct := fanPercentReading(readings, primary)
	if pct == nil || pct.ID != "superio/control/1" {
		t.Fatalf("占空比读数选错: %+v", pct)
	}
	if pct.Value != 39 {
		t.Errorf("占空比 = %v，期望 39", pct.Value)
	}

	// 3) 整条采集链路：快照里的主风扇与占空比都要正确
	c := NewCollector(10)
	c.AddSource(fakeSource{readings: readings})
	c.refresh(context.Background())
	snap := c.Snapshot()
	if snap.Fan == nil || snap.Fan.ID != "superio/fan/1" {
		t.Fatalf("快照里的主风扇 = %+v，期望 superio/fan/1", snap.Fan)
	}
	if snap.FanPercent == nil || snap.FanPercent.Value != 39 {
		t.Fatalf("快照里的占空比 = %+v，期望 39", snap.FanPercent)
	}
	if snap.CPUTemperature == nil || snap.CPUTemperature.Value != 34.5 {
		t.Fatalf("快照里的 CPU 温度 = %+v，期望 34.5", snap.CPUTemperature)
	}

	// 4) 调速通道要自动对齐到通道 1
	backend := &fakeFanBackend{available: true, channels: 8}
	fan := NewFanController(backend)
	c.SetFanController(fan)
	c.refresh(context.Background())
	if fan.State().Channel != 1 {
		t.Errorf("自动对齐的调速通道 = %v，期望 1", fan.State().Channel)
	}
}

// TestSuperIOReplayNEC8UserLocksChannel 复现"用户选了通道却被自动改回去"。
func TestSuperIOReplayNEC8UserLocksChannel(t *testing.T) {
	readings := []Reading{
		{ID: "superio/fan/0", Name: "通道 0 · CPU 风扇", Category: catFan, Unit: "RPM", Value: 0,
			Hardware: "Nuvoton NCT6686D"},
		{ID: "superio/fan/1", Name: "通道 1 · PUMP 风扇", Category: catFan, Unit: "RPM", Value: 1015,
			Hardware: "Nuvoton NCT6686D"},
	}

	backend := &fakeFanBackend{available: true, channels: 8}
	fan := NewFanController(backend)
	if err := fan.SetChannel(0); err != nil {
		t.Fatalf("设置通道失败: %v", err)
	}

	c := NewCollector(10)
	c.SetFanController(fan)
	c.AddSource(fakeSource{readings: readings})
	for i := 0; i < 3; i++ {
		c.refresh(context.Background())
	}
	if fan.State().Channel != 0 {
		t.Errorf("用户选的通道 0 被改成了 %v", fan.State().Channel)
	}
}

func TestPadRightUsesDisplayWidth(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"CPU", 10, "CPU       "},
		{"通道 1 · PUMP 风扇", 20, "通道 1 · PUMP 风扇  "},
		{"通道 0 · CPU 风扇", 20, "通道 0 · CPU 风扇   "},
	}
	for _, tc := range cases {
		got := padRight(tc.in, tc.width)
		if got != tc.want {
			t.Errorf("padRight(%q, %d) = %q，期望 %q", tc.in, tc.width, got, tc.want)
		}
		if displayWidth(got) != tc.width && displayWidth(tc.in) < tc.width {
			t.Errorf("padRight(%q, %d) 显示宽度 = %d，期望 %d", tc.in, tc.width, displayWidth(got), tc.width)
		}
	}
}

func TestParseValue(t *testing.T) {
	cases := []struct {
		in    string
		value float64
		unit  string
		ok    bool
	}{
		{"45.6 °C", 45.6, "°C", true},
		{"1180 RPM", 1180, "RPM", true},
		{"1.176 V", 1.176, "V", true},
		{"18.3 %", 18.3, "%", true},
		{"", 0, "", false},
		{"N/A", 0, "", false},
		{"-3.5 °C", -3.5, "°C", true},
	}
	for _, c := range cases {
		v, u, ok := parseValue(c.in)
		if ok != c.ok {
			t.Errorf("parseValue(%q) ok = %v，期望 %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if v != c.value || u != c.unit {
			t.Errorf("parseValue(%q) = (%v, %q)，期望 (%v, %q)", c.in, v, u, c.value, c.unit)
		}
	}
}

func TestCollectorSnapshot(t *testing.T) {
	c := NewCollector(10)
	c.AddSource(fakeSource{readings: loadSample(t)})
	c.refresh(context.Background())

	snap := c.Snapshot()
	if snap.CPUTemperature == nil || snap.Fan == nil {
		t.Fatal("快照里没有 CPU 温度或风扇")
	}
	if len(snap.Readings) != 15 {
		t.Errorf("快照读数 = %d，期望 15", len(snap.Readings))
	}
	if len(snap.Sources) != 1 || !snap.Sources[0].OK {
		t.Errorf("数据源状态异常: %+v", snap.Sources)
	}
	if len(c.History()) != 1 {
		t.Errorf("历史点数 = %d，期望 1", len(c.History()))
	}
}

// fakeSource 用于测试的固定数据源。
type fakeSource struct {
	readings []Reading
	err      error
}

func (f fakeSource) name() string { return "fake" }

func (f fakeSource) collect(_ context.Context) ([]Reading, error) { return f.readings, f.err }

func TestFanPercentReading(t *testing.T) {
	readings := loadSample(t)
	pct := fanPercentReading(readings, pickPrimaryFan(readings))
	if pct == nil {
		t.Fatal("没有找到风扇调速百分比")
	}
	// 样例里的 "Smart Fan" 属于 Control 类别、单位 %
	if pct.Value != 38.0 {
		t.Errorf("调速百分比 = %v，期望 38", pct.Value)
	}
	if pct.Category != "Control" {
		t.Errorf("调速百分比类别 = %q，期望 Control", pct.Category)
	}
	// 必须与主风扇同属一块硬件（Nuvoton），不能拿到显卡风扇的 25%
	if pct.Hardware != "Nuvoton NCT6798" {
		t.Errorf("调速百分比硬件 = %q，期望 Nuvoton NCT6798", pct.Hardware)
	}
}

// TestFanPercentReadingSameChannel 占空比必须取主风扇"同一个通道"的，
// 不能拿到同一块芯片上别的通道的值。
func TestFanPercentReadingSameChannel(t *testing.T) {
	readings := []Reading{
		{ID: "superio/fan/0", Name: "通道 0 · CPU 风扇", Category: catFan, Unit: "RPM", Value: 0,
			Hardware: "Nuvoton NCT6686D"},
		{ID: "superio/control/0", Name: "通道 0 · CPU 风扇 占空比", Category: "Control", Unit: "%", Value: 0,
			Hardware: "Nuvoton NCT6686D"},
		{ID: "superio/fan/1", Name: "通道 1 · PUMP 风扇", Category: catFan, Unit: "RPM", Value: 1015,
			Hardware: "Nuvoton NCT6686D"},
		{ID: "superio/control/1", Name: "通道 1 · PUMP 风扇 占空比", Category: "Control", Unit: "%", Value: 39,
			Hardware: "Nuvoton NCT6686D"},
	}
	primary := pickPrimaryFan(readings)
	if primary == nil || primary.ID != "superio/fan/1" {
		t.Fatalf("主风扇选错: %+v", primary)
	}
	pct := fanPercentReading(readings, primary)
	if pct == nil {
		t.Fatal("没有找到占空比读数")
	}
	if pct.ID != "superio/control/1" {
		t.Errorf("占空比读数 = %s，期望 superio/control/1", pct.ID)
	}
	if pct.Value != 39 {
		t.Errorf("占空比 = %v，期望 39", pct.Value)
	}
}
