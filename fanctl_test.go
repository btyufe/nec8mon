package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeFanBackend 记录调速调用，用于验证控制器的行为。
type fakeFanBackend struct {
	available bool
	channels  int
	duty      float64
	setCalls  []float64
	restores  int
	lastErr   error
}

func (f *fakeFanBackend) Info() FanBackendInfo {
	return FanBackendInfo{
		Available: f.available,
		Kind:      "fake",
		Chip:      "Fake Super I/O",
		Detail:    "单元测试后端",
		Channels:  f.channels,
	}
}

func (f *fakeFanBackend) SetManual(channel int, percent float64) error {
	if f.lastErr != nil {
		return f.lastErr
	}
	f.duty = percent
	f.setCalls = append(f.setCalls, percent)
	return nil
}

func (f *fakeFanBackend) Restore(channel int) error {
	f.restores++
	f.duty = 0
	return nil
}

func (f *fakeFanBackend) Duty(channel int) (float64, bool) {
	return f.duty, true
}

func TestFanCurveEvaluation(t *testing.T) {
	c := NewFanController(&fakeFanBackend{available: true, channels: 1})
	c.curve = FanCurve{
		Points: []FanCurvePoint{
			{Temp: 30, Duty: 20},
			{Temp: 60, Duty: 60},
			{Temp: 90, Duty: 100},
		},
	}

	cases := []struct {
		temp float64
		want float64
	}{
		{20, 20},   // 低于最低点，取最低点
		{30, 20},   // 正好在最低点
		{45, 40},   // 中点线性插值
		{60, 60},   // 中间点
		{75, 80},   // 后半段插值
		{90, 100},  // 正好在最高点
		{120, 100}, // 高于最高点
	}
	for _, tc := range cases {
		got := c.evaluateCurve(tc.temp)
		if got < tc.want-0.001 || got > tc.want+0.001 {
			t.Errorf("evaluateCurve(%v) = %v，期望 %v", tc.temp, got, tc.want)
		}
	}
}

func TestFanCurveUnsortedPoints(t *testing.T) {
	c := NewFanController(&fakeFanBackend{available: true, channels: 1})
	c.curve = FanCurve{
		Points: []FanCurvePoint{
			{Temp: 60, Duty: 60},
			{Temp: 30, Duty: 20},
			{Temp: 90, Duty: 100},
		},
	}
	if got := c.evaluateCurve(45); got < 39.9 || got > 40.1 {
		t.Errorf("乱序曲线在 45°C 的取值 = %v，期望 40", got)
	}
}

func TestFanManualModeWritesDuty(t *testing.T) {
	backend := &fakeFanBackend{available: true, channels: 2}
	c := NewFanController(backend)

	if err := c.SetMode(FanModeManual); err != nil {
		t.Fatalf("切换手动模式失败: %v", err)
	}
	if err := c.SetDuty(45); err != nil {
		t.Fatalf("设置占空比失败: %v", err)
	}
	if len(backend.setCalls) == 0 {
		t.Fatal("没有向硬件写入占空比")
	}
	if backend.setCalls[len(backend.setCalls)-1] != 45 {
		t.Errorf("最后一次写入的占空比 = %v，期望 45", backend.setCalls[len(backend.setCalls)-1])
	}
	if c.State().AppliedDuty != 45 {
		t.Errorf("状态里的已写入占空比 = %v，期望 45", c.State().AppliedDuty)
	}
}

func TestFanDutyLowerBoundRejected(t *testing.T) {
	backend := &fakeFanBackend{available: true, channels: 1}
	c := NewFanController(backend)
	if err := c.SetDuty(5); err == nil {
		t.Error("低于安全下限的占空比应当被拒绝")
	}
	if err := c.SetDuty(101); err == nil {
		t.Error("超过 100% 的占空比应当被拒绝")
	}
}

func TestFanAutoModeRestoresHardware(t *testing.T) {
	backend := &fakeFanBackend{available: true, channels: 1}
	c := NewFanController(backend)

	_ = c.SetMode(FanModeManual)
	_ = c.SetDuty(40)
	if err := c.SetMode(FanModeAuto); err != nil {
		t.Fatalf("切回自动模式失败: %v", err)
	}
	if backend.restores == 0 {
		t.Error("切回自动模式时应当把风扇交还硬件")
	}
	if c.State().Mode != FanModeAuto {
		t.Errorf("状态里的模式 = %v，期望 auto", c.State().Mode)
	}
}

func TestFanSafetyOverrideOnHighTemp(t *testing.T) {
	backend := &fakeFanBackend{available: true, channels: 1}
	c := NewFanController(backend)

	_ = c.SetMode(FanModeManual)
	_ = c.SetDuty(25)
	_ = c.SetSafetyTemp(70)

	c.Tick(40, true)
	if backend.duty != 25 {
		t.Fatalf("低温时占空比 = %v，期望 25", backend.duty)
	}

	// 温度越线后必须强制满速
	c.Tick(85, true)
	if backend.duty != 100 {
		t.Errorf("过温时占空比 = %v，期望 100", backend.duty)
	}
	if !c.State().Emergency {
		t.Error("过温时应当标记 emergency")
	}
}

func TestFanCurveModeFollowsTemperature(t *testing.T) {
	backend := &fakeFanBackend{available: true, channels: 1}
	c := NewFanController(backend)
	c.curve = FanCurve{
		Points: []FanCurvePoint{
			{Temp: 30, Duty: 20},
			{Temp: 90, Duty: 100},
		},
	}
	_ = c.SetMode(FanModeCurve)
	// 把过温阈值调高，避免干扰曲线本身的验证
	_ = c.SetSafetyTemp(95)

	c.Tick(60, true)
	// 60°C 处在 30~90 的中点，目标应为 60%
	if backend.duty < 59 || backend.duty > 61 {
		t.Errorf("曲线模式在 60°C 的占空比 = %v，期望约 60", backend.duty)
	}

	c.Tick(85, true)
	// 30°C→20%、90°C→100%，85°C 落在 91.7% 附近
	if backend.duty < 92 || backend.duty > 95 {
		t.Errorf("曲线模式在 85°C 的占空比 = %v，期望约 93", backend.duty)
	}
}

func TestFanUnavailableBackendRejectsManual(t *testing.T) {
	c := NewFanController(nil)
	if err := c.SetMode(FanModeManual); err == nil {
		t.Error("后端不可用时切换到手动模式应当报错")
	}
	if c.Available() {
		t.Error("后端为 nil 时不应报告可用")
	}
}

func TestFanControlAPI(t *testing.T) {
	backend := &fakeFanBackend{available: true, channels: 3}
	fan := NewFanController(backend)
	collector := NewCollector(10)

	srv := httptest.NewServer(newServer(collector, fan).routes())
	defer srv.Close()

	// GET 状态
	resp, err := http.Get(srv.URL + "/api/fan")
	if err != nil {
		t.Fatalf("GET /api/fan 失败: %v", err)
	}
	var st FanState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("解析状态失败: %v", err)
	}
	resp.Body.Close()
	if !st.Available {
		t.Error("状态应当报告调速可用")
	}
	if st.Backend.Channels != 3 {
		t.Errorf("状态里的通道数 = %v，期望 3", st.Backend.Channels)
	}

	// POST 切到手动并设置占空比
	body := strings.NewReader(`{"mode":"manual","duty":42}`)
	resp, err = http.Post(srv.URL+"/api/fan/control", "application/json", body)
	if err != nil {
		t.Fatalf("POST /api/fan/control 失败: %v", err)
	}
	var st2 FanState
	if err := json.NewDecoder(resp.Body).Decode(&st2); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %v，期望 200", resp.StatusCode)
	}
	if st2.Mode != FanModeManual {
		t.Errorf("模式 = %v，期望 manual", st2.Mode)
	}
	if st2.Duty != 42 {
		t.Errorf("占空比 = %v，期望 42", st2.Duty)
	}
	if len(backend.setCalls) == 0 || backend.setCalls[len(backend.setCalls)-1] != 42 {
		t.Errorf("硬件未收到 42%% 的写入: %v", backend.setCalls)
	}

	// 非法请求体应当被拒绝
	resp, err = http.Post(srv.URL+"/api/fan/control", "application/json", strings.NewReader(`{"duty":5}`))
	if err != nil {
		t.Fatalf("POST 失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法占空比的状态码 = %v，期望 400", resp.StatusCode)
	}
}

func TestParseFanSetSpec(t *testing.T) {
	cases := []struct {
		in      string
		ch      int
		duty    float64
		wantErr bool
	}{
		{"1:40", 1, 40, false},
		{" 0 : 100 ", 0, 100, false},
		{"3:20", 3, 20, false},
		{"1", 0, 0, true},
		{"1:40:50", 0, 0, true},
		{"x:40", 0, 0, true},
		{"1:y", 0, 0, true},
		{"-1:40", 0, 0, true},
		{"1:5", 0, 0, true},   // 低于安全下限
		{"1:120", 0, 0, true}, // 超过 100
	}
	for _, tc := range cases {
		ch, duty, err := parseFanSetSpec(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseFanSetSpec(%q) 应当报错", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseFanSetSpec(%q) 报错: %v", tc.in, err)
			continue
		}
		if ch != tc.ch || duty != tc.duty {
			t.Errorf("parseFanSetSpec(%q) = (%v, %v)，期望 (%v, %v)", tc.in, ch, duty, tc.ch, tc.duty)
		}
	}
}

func TestChannelFromFanID(t *testing.T) {
	cases := []struct {
		id   string
		want int
		ok   bool
	}{
		{"/lpc/nct6686d/0/fan/0", 0, true},
		{"/lpc/nct6798d/0/fan/2", 2, true},
		{"/lpc/nct6686d/0/control/5", 5, true},
		{"/intelcpu/0/temperature/22", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := channelFromFanID(tc.id)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("channelFromFanID(%q) = (%v, %v)，期望 (%v, %v)", tc.id, got, ok, tc.want, tc.ok)
		}
	}
}

func TestAutoChannelFollowsPrimaryFan(t *testing.T) {
	backend := &fakeFanBackend{available: true, channels: 6}
	c := NewFanController(backend)

	readings := []Reading{
		{ID: "/lpc/nct6686d/0/fan/3", Name: "CPU Fan", Category: catFan, Unit: "RPM", Value: 1200},
	}
	primary := pickPrimaryFan(readings)
	if primary == nil {
		t.Fatal("没有识别出主风扇")
	}
	c.AutoChannel(readings, primary)
	if c.State().Channel != 3 {
		t.Errorf("自动推断的通道 = %v，期望 3", c.State().Channel)
	}
}

// TestAutoChannelPrefersRunningFan 复现现场故障：
// 通道 0 名字叫"CPU 风扇"但没接风扇（恒 0 RPM），真正在转的是通道 1。
// 自动对齐必须跟着"在转的"那个走。
func TestAutoChannelPrefersRunningFan(t *testing.T) {
	backend := &fakeFanBackend{available: true, channels: 8}
	c := NewFanController(backend)

	readings := []Reading{
		{ID: "superio/fan/0", Name: "通道 0 · CPU 风扇", Category: catFan, Unit: "RPM", Value: 0},
		{ID: "superio/fan/1", Name: "通道 1 · PUMP 风扇", Category: catFan, Unit: "RPM", Value: 1015},
	}
	c.AutoChannel(readings, pickPrimaryFan(readings))
	if c.State().Channel != 1 {
		t.Errorf("自动推断的通道 = %v，期望 1（在转的那个）", c.State().Channel)
	}
}

// TestAutoChannelKeepsUserChoice 用户手动选过通道之后，
// 采集循环不能再把通道改回去——否则用户的选择转眼就没了。
func TestAutoChannelKeepsUserChoice(t *testing.T) {
	backend := &fakeFanBackend{available: true, channels: 8}
	c := NewFanController(backend)

	if err := c.SetChannel(5); err != nil {
		t.Fatalf("设置通道失败: %v", err)
	}
	readings := []Reading{
		{ID: "superio/fan/0", Name: "通道 0 · CPU 风扇", Category: catFan, Unit: "RPM", Value: 0},
		{ID: "superio/fan/1", Name: "通道 1 · PUMP 风扇", Category: catFan, Unit: "RPM", Value: 1015},
	}
	c.AutoChannel(readings, pickPrimaryFan(readings))
	if c.State().Channel != 5 {
		t.Errorf("用户选的通道被改成了 %v，期望保持 5", c.State().Channel)
	}
	if !c.State().ChannelLocked {
		t.Error("手动选过通道后应当标记为已锁定")
	}
}

// TestSetModeManualAppliesImmediately 切到手动模式要立刻写入占空比，
// 不能等下一轮采集（否则用户会以为"没反应"）。
func TestSetModeManualAppliesImmediately(t *testing.T) {
	backend := &fakeFanBackend{available: true, channels: 2}
	c := NewFanController(backend)
	c.duty = 35

	if err := c.SetMode(FanModeManual); err != nil {
		t.Fatalf("切换手动模式失败: %v", err)
	}
	if len(backend.setCalls) == 0 {
		t.Fatal("切到手动模式时应当立刻写入占空比")
	}
	if backend.setCalls[len(backend.setCalls)-1] != 35 {
		t.Errorf("写入的占空比 = %v，期望 35", backend.setCalls[len(backend.setCalls)-1])
	}
}

// TestSetChannelAppliesToNewChannel 手动模式下换通道要立刻写新通道。
func TestSetChannelAppliesToNewChannel(t *testing.T) {
	backend := &fakeFanBackend{available: true, channels: 4}
	c := NewFanController(backend)
	_ = c.SetMode(FanModeManual)
	_ = c.SetDuty(30)

	if err := c.SetChannel(2); err != nil {
		t.Fatalf("切换通道失败: %v", err)
	}
	if backend.duty != 30 {
		t.Errorf("新通道上的占空比 = %v，期望 30", backend.duty)
	}
	if c.State().Channel != 2 {
		t.Errorf("当前通道 = %v，期望 2", c.State().Channel)
	}
}

// TestAutoChannelRestoresOldChannel 换通道时要把旧通道交还硬件。
func TestAutoChannelRestoresOldChannel(t *testing.T) {
	backend := &fakeFanBackend{available: true, channels: 8}
	c := NewFanController(backend)
	_ = c.SetMode(FanModeManual)
	_ = c.SetDuty(30)

	readings := []Reading{
		{ID: "superio/fan/1", Name: "通道 1 · PUMP 风扇", Category: catFan, Unit: "RPM", Value: 1015},
	}
	c.AutoChannel(readings, pickPrimaryFan(readings))
	if backend.restores == 0 {
		t.Error("自动切换通道时应当把旧通道交还硬件")
	}
}

// TestAutoChannelKeepsChannelWhenAllStopped 全部风扇停转时不要改通道，
// 否则风扇一停，控制目标就被切到别处去了。
func TestAutoChannelKeepsChannelWhenAllStopped(t *testing.T) {
	backend := &fakeFanBackend{available: true, channels: 8}
	c := NewFanController(backend)
	if err := c.SetChannel(3); err != nil {
		t.Fatalf("设置通道失败: %v", err)
	}

	readings := []Reading{
		{ID: "superio/fan/0", Name: "通道 0 · CPU 风扇", Category: catFan, Unit: "RPM", Value: 0},
	}
	c.AutoChannel(readings, pickPrimaryFan(readings))
	if c.State().Channel != 3 {
		t.Errorf("全部停转时通道被改成了 %v，期望保持 3", c.State().Channel)
	}
}

func TestCollectorDrivesFanController(t *testing.T) {
	backend := &fakeFanBackend{available: true, channels: 1}
	fan := NewFanController(backend)
	_ = fan.SetMode(FanModeManual)
	_ = fan.SetDuty(30)

	collector := NewCollector(10)
	collector.SetFanController(fan)
	collector.AddSource(fakeSource{readings: []Reading{
		{ID: "/intelcpu/0/temperature/0", Name: "CPU Package", Category: catTemperature,
			Unit: "°C", Value: 42, Hardware: "Intel Core i5-8500T"},
		{ID: "/lpc/nct6686d/0/fan/0", Name: "CPU Fan", Category: catFan,
			Unit: "RPM", Value: 900, Hardware: "Nuvoton NCT6686D"},
	}})

	collector.refresh(context.Background())

	if len(backend.setCalls) == 0 {
		t.Fatal("采集后应当驱动风扇控制器写入占空比")
	}
	if backend.setCalls[len(backend.setCalls)-1] != 30 {
		t.Errorf("写入的占空比 = %v，期望 30", backend.setCalls[len(backend.setCalls)-1])
	}
}

func TestFanCurveHysteresis(t *testing.T) {
	c := NewFanController(&fakeFanBackend{available: true, channels: 1})
	c.curve = FanCurve{
		Points: []FanCurvePoint{
			{Temp: 50, Duty: 30},
			{Temp: 70, Duty: 70},
		},
		Hysteresis: 3,
	}

	// 升温到 60°C -> 占空比 50%
	d1 := c.evaluateCurve(60)
	if d1 < 49.9 || d1 > 50.1 {
		t.Fatalf("60°C 占空比 = %v，期望 50", d1)
	}

	// 微幅降温到 58.5°C（降温 1.5°C < 回差 3°C）-> 应保持 50%，不降速
	d2 := c.evaluateCurve(58.5)
	if d2 != d1 {
		t.Errorf("回差保护期间占空比 = %v，期望维持 %v", d2, d1)
	}

	// 进一步降温到 55°C（降温 5°C > 回差 3°C）-> 占空比应正常回落到 40%
	d3 := c.evaluateCurve(55)
	if d3 < 39.9 || d3 > 40.1 {
		t.Errorf("降温脱离回差后占空比 = %v，期望 40", d3)
	}
}

func TestFanWatchdogReapply(t *testing.T) {
	backend := &fakeFanBackend{available: true, channels: 1, duty: 40}
	c := NewFanController(backend)
	_ = c.SetMode(FanModeManual)
	_ = c.SetDuty(40)
	initialCalls := len(backend.setCalls)

	// 正常情况：hardwareDuty 与目标一致（40），Tick 不重复强刷
	c.Tick(45, true)
	if len(backend.setCalls) != initialCalls {
		t.Fatalf("转速一致时不应强刷寄存器")
	}

	// 模拟外部（BIOS）将芯片改回了 30%
	backend.duty = 30
	c.Tick(45, true)
	if len(backend.setCalls) <= initialCalls {
		t.Errorf("检测到 BIOS 篡改时 Watchdog 应强制重新夺回占空比")
	}
}
