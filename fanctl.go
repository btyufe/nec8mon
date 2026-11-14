package main

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"
)

// 风扇控制的安全下限：占空比低于 20% 时部分风扇会停转，风险偏高。
const minSafeDutyPercent = 20.0

// channelFromFanID 从 LibreHardwareMonitor 的传感器标识符里解析出风扇通道号。
//
// 形如 "/lpc/nct6686d/0/fan/0" 的标识符，最后一段就是通道索引。
func channelFromFanID(id string) (int, bool) {
	parts := strings.Split(strings.Trim(id, "/"), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == "fan" || parts[i] == "control" {
			if i+1 < len(parts) {
				var n int
				if _, err := fmt.Sscanf(parts[i+1], "%d", &n); err == nil {
					return n, true
				}
			}
			return 0, false
		}
	}
	return 0, false
}

// FanMode 表示风扇控制模式。
type FanMode string

const (
	// FanModeAuto 把风扇完全交还 BIOS / 硬件自动控制（默认，最安全）。
	FanModeAuto FanMode = "auto"
	// FanModeManual 固定一个占空比，由用户手动设定。
	FanModeManual FanMode = "manual"
	// FanModeCurve 按 CPU 温度自动调节占空比。
	FanModeCurve FanMode = "curve"
)

// FanCurvePoint 是曲线上的一个点：温度达到 Temp 时目标占空比为 Duty。
type FanCurvePoint struct {
	Temp float64 `json:"temp"`
	Duty float64 `json:"duty"`
}

// FanCurve 描述温度到占空比的映射关系。
type FanCurve struct {
	Points []FanCurvePoint `json:"points"`
	// Hysteresis 是回差，单位摄氏度，避免在临界点来回抖动。
	Hysteresis float64 `json:"hysteresis"`
}

// DefaultQuietCurve 是默认的"安静曲线"：
// 低温时低速运行，温度上来再逐步拉高，适合冬天或轻负载。
func DefaultQuietCurve() FanCurve {
	return FanCurve{
		Points: []FanCurvePoint{
			{Temp: 30, Duty: 20},
			{Temp: 45, Duty: 22},
			{Temp: 60, Duty: 35},
			{Temp: 70, Duty: 55},
			{Temp: 80, Duty: 85},
			{Temp: 88, Duty: 100},
		},
		Hysteresis: 3,
	}
}

// FanBackendInfo 描述调速后端的能力与状态。
type FanBackendInfo struct {
	Available bool   `json:"available"`
	Kind      string `json:"kind"`
	Chip      string `json:"chip"`
	Detail    string `json:"detail"`
	Channels  int    `json:"channels"`
}

// FanState 是对外暴露的风扇控制状态。
type FanState struct {
	Available  bool           `json:"available"`
	Backend    FanBackendInfo `json:"backend"`
	Mode       FanMode        `json:"mode"`
	Duty       float64        `json:"duty"`
	Curve      FanCurve       `json:"curve"`
	SafetyTemp float64        `json:"safetyTemp"`
	Channel    int            `json:"channel"`
	// ChannelLocked 为真表示通道由用户手动指定，采集循环不会再自动改。
	ChannelLocked bool    `json:"channelLocked"`
	Emergency     bool    `json:"emergency"`
	AppliedDuty   float64 `json:"appliedDuty"`
	// HardwareDuty 是从芯片寄存器读回的当前占空比。
	HardwareDuty float64   `json:"hardwareDuty"`
	HasDuty      bool      `json:"hasDuty"`
	Testing      bool      `json:"testing"`
	LastError    string    `json:"lastError"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// fanBackend 是可调速风扇的底层抽象。
type fanBackend interface {
	// Info 描述后端状态。
	Info() FanBackendInfo
	// SetManual 接管通道并写入占空比（0-100）。
	SetManual(channel int, percent float64) error
	// Restore 把通道交还硬件自动控制。
	Restore(channel int) error
	// Duty 读取当前占空比（0-100），失败时返回 false。
	Duty(channel int) (float64, bool)
}

// leftoverRestorer 由支持还原点的后端实现。
//
// 用于在启动时收拾"上次进程异常退出、芯片被留在手动模式"的残局。
type leftoverRestorer interface {
	RestoreLeftover() error
}

// FanController 管理风扇调速：模式切换、曲线计算、过温保护。
type FanController struct {
	mu      sync.Mutex
	backend fanBackend

	mode       FanMode
	duty       float64
	curve      FanCurve
	safetyTemp float64
	channel    int
	// channelLocked 表示用户手动选过通道，此后不再自动跟随主风扇。
	// 没有这个标志的话，每轮采集都会把通道拉回"主风扇"，
	// 用户在界面上选的通道转眼就被覆盖，看起来就像"调速没反应"。
	channelLocked bool

	// lastTemp / hasTemp 记录最近一次采集到的温度，
	// 切换到曲线模式时可以立刻用上，不必等下一轮采集。
	lastTemp float64
	hasTemp  bool

	// 用于温度曲线回差防抖
	lastCurveDuty float64
	lastEvalTemp  float64
	hasCurveDuty  bool

	// statePath 是设置文件的路径。为空时不落盘（仅监控模式）。
	statePath string

	appliedDuty  float64
	hardwareDuty float64
	hasDuty      bool
	emergency    bool
	testing      bool
	lastError    string
	updated      time.Time
}

// NewFanController 创建一个控制器。backend 为 nil 时所有调速操作都会返回明确错误。
func NewFanController(backend fanBackend) *FanController {
	return &FanController{
		backend:    backend,
		mode:       FanModeAuto,
		duty:       30,
		curve:      DefaultQuietCurve(),
		safetyTemp: 75,
		channel:    0,
		updated:    time.Now(),
	}
}

// EnablePersistence 打开设置持久化。
//
// 打开之后，用户在网页上做的每一项调整都会立刻写进 statePath 指向的文件；
// 服务重启或机器重启后由 RestoreState 读回来，风扇转速设置不会莫名其妙
// 变回 BIOS 默认值。
func (c *FanController) EnablePersistence(statePath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statePath = statePath
}

// RestoreState 从 statePath 读回上次的设置并应用。
//
// 只在"确实存在设置文件"时才动手；读文件失败或内容不合法时保持当前状态，
// 绝不因为存档损坏就让风扇失控。
func (c *FanController) RestoreState(statePath string) error {
	st, ok, err := loadFanState(statePath)
	if err != nil {
		return fmt.Errorf("读取风扇设置失败: %w", err)
	}
	if !ok {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// 逐项校验，坏的值直接丢弃而不是让整个存档作废。
	switch st.Mode {
	case FanModeManual, FanModeCurve:
		c.mode = st.Mode
	case FanModeAuto:
		c.mode = FanModeAuto
	}
	if st.Duty >= minSafeDutyPercent && st.Duty <= 100 {
		c.duty = st.Duty
	}
	if st.SafetyTemp >= 50 && st.SafetyTemp <= 110 {
		c.safetyTemp = st.SafetyTemp
	}
	if len(st.Curve.Points) > 0 {
		valid := true
		for _, p := range st.Curve.Points {
			if p.Duty < minSafeDutyPercent || p.Duty > 100 {
				valid = false
				break
			}
		}
		if valid {
			c.curve = st.Curve
		}
	}
	if st.Channel >= 0 && c.backend != nil && st.Channel < c.backend.Info().Channels {
		c.channel = st.Channel
	}
	c.channelLocked = st.ChannelLocked
	c.updated = time.Now()

	// 把恢复出来的设置真正写进硬件：手动模式立刻生效；
	// 曲线模式要等第一次采集拿到温度才能算出目标占空比。
	if c.mode == FanModeManual {
		if err := c.applyLocked(c.duty, true); err != nil {
			return fmt.Errorf("恢复手动占空比失败: %w", err)
		}
	}
	return nil
}

// persistLocked 把当前设置写进设置文件。调用方必须已持有锁。
//
// 写盘失败不影响调速本身，只记一条日志——设置丢失总比风扇失控强。
func (c *FanController) persistLocked() {
	if c.statePath == "" {
		return
	}
	st := fanState{
		Mode:          c.mode,
		Channel:       c.channel,
		ChannelLocked: c.channelLocked,
		Duty:          c.duty,
		Curve:         c.curve,
		SafetyTemp:    c.safetyTemp,
	}
	if err := saveFanState(c.statePath, st); err != nil {
		log.Printf("[fan] 保存风扇设置失败: %v", err)
	}
}

// Info 返回后端信息。
func (c *FanController) Info() FanBackendInfo {
	if c.backend == nil {
		return FanBackendInfo{Available: false, Kind: "none"}
	}
	return c.backend.Info()
}

// State 返回当前控制状态。
func (c *FanController) State() FanState {
	c.mu.Lock()
	defer c.mu.Unlock()

	info := FanBackendInfo{Kind: "none"}
	if c.backend != nil {
		info = c.backend.Info()
	}
	return FanState{
		Available:     info.Available,
		Backend:       info,
		Mode:          c.mode,
		Duty:          c.duty,
		Curve:         c.curve,
		SafetyTemp:    c.safetyTemp,
		Channel:       c.channel,
		ChannelLocked: c.channelLocked,
		Emergency:     c.emergency,
		AppliedDuty:   c.appliedDuty,
		HardwareDuty:  c.hardwareDuty,
		HasDuty:       c.hasDuty,
		Testing:       c.testing,
		LastError:     c.lastError,
		UpdatedAt:     c.updated,
	}
}

// Available 表示后端是否可用。
func (c *FanController) Available() bool {
	return c.backend != nil && c.backend.Info().Available
}

// SetMode 切换控制模式。
func (c *FanController) SetMode(mode FanMode) error {
	switch mode {
	case FanModeAuto, FanModeManual, FanModeCurve:
	default:
		return fmt.Errorf("未知的控制模式 %q", mode)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if mode == FanModeAuto {
		if c.backend != nil && c.mode != FanModeAuto {
			if err := c.backend.Restore(c.channel); err != nil {
				c.lastError = err.Error()
				return fmt.Errorf("交还硬件控制失败: %w", err)
			}
		}
		c.mode = FanModeAuto
		c.emergency = false
		c.appliedDuty = 0
		c.lastError = ""
		c.updated = time.Now()
		c.persistLocked()
		return nil
	}

	if !c.Available() {
		return fmt.Errorf("调速后端不可用：%s", c.backendDetail())
	}
	c.mode = mode
	c.emergency = false
	c.lastError = ""
	c.updated = time.Now()

	// 立刻把当前目标占空比写下去：用户点一下"手动固定转速"就希望能马上
	// 看到转速变化，而不是等下一轮采集（最多 2 秒）才生效。
	if target, ok := c.targetDutyLocked(); ok {
		if err := c.applyLocked(target, true); err != nil {
			return fmt.Errorf("切换到%s成功，但写入占空比失败: %w", describeMode(mode), err)
		}
	}
	c.persistLocked()
	return nil
}

// SetDuty 设置手动模式下的目标占空比。
func (c *FanController) SetDuty(percent float64) error {
	if percent < minSafeDutyPercent || percent > 100 {
		return fmt.Errorf("占空比需在 %d%%-100%% 之间", int(minSafeDutyPercent))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.duty = percent
	c.updated = time.Now()

	if c.mode == FanModeManual {
		if err := c.applyLocked(percent, true); err != nil {
			return err
		}
	}
	c.persistLocked()
	return nil
}

// SetCurve 设置温度曲线。
func (c *FanController) SetCurve(curve FanCurve) error {
	if len(curve.Points) == 0 {
		return fmt.Errorf("曲线至少需要一个点")
	}
	for i, p := range curve.Points {
		if p.Duty < minSafeDutyPercent || p.Duty > 100 {
			return fmt.Errorf("曲线第 %d 点的占空比需在 %d%%-100%% 之间", i+1, int(minSafeDutyPercent))
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.curve = curve
	c.updated = time.Now()
	c.persistLocked()
	return nil
}

// SetSafetyTemp 设置过温保护阈值。
func (c *FanController) SetSafetyTemp(temp float64) error {
	if temp < 50 || temp > 110 {
		return fmt.Errorf("过温阈值需在 50-110 °C 之间")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.safetyTemp = temp
	c.updated = time.Now()
	c.persistLocked()
	return nil
}

// SetChannel 指定要控制的风扇通道。
//
// 选定之后这个通道就被"锁定"，采集循环不会再自动改回去。
func (c *FanController) SetChannel(channel int) error {
	if !c.Available() {
		return fmt.Errorf("调速后端不可用")
	}
	channels := c.backend.Info().Channels
	if channel < 0 || channel >= channels {
		return fmt.Errorf("风扇通道需在 0-%d 之间", channels-1)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channelLocked = true

	if c.channel != channel {
		if c.mode != FanModeAuto && c.backend != nil {
			_ = c.backend.Restore(c.channel)
		}
		c.channel = channel
		c.appliedDuty = 0

		// 换通道后立刻把目标占空比写到新通道上，
		// 否则要等下一轮 Tick 才生效，看起来就像"没反应"。
		if c.mode != FanModeAuto {
			if target, ok := c.targetDutyLocked(); ok {
				if err := c.applyLocked(target, true); err != nil {
					return err
				}
			}
		}
	}
	c.updated = time.Now()
	c.persistLocked()
	return nil
}

// activeFanChannel 找出当前真正在转的风扇通道。
//
// 只靠名字判断不可靠：现场那台 M720q 上"CPU 风扇"位置根本没接风扇，
// 真正在转的是另一个通道。这里按转速来选，返回转速最高的那个通道。
func activeFanChannel(readings []Reading) (int, bool) {
	best := -1
	bestRPM := 0.0
	for i := range readings {
		r := readings[i]
		if r.Category != catFan || !strings.EqualFold(r.Unit, "RPM") {
			continue
		}
		if isGPUish(r) {
			continue
		}
		ch, ok := channelFromFanID(r.ID)
		if !ok {
			continue
		}
		if r.Value > bestRPM {
			bestRPM, best = r.Value, ch
		}
	}
	if best < 0 || bestRPM <= 0 {
		return 0, false
	}
	return best, true
}

// AutoChannel 尝试从传感器读数里推断应当控制哪个风扇通道。
//
// 优先选"真正在转"的那个通道；找不到在转的通道时退回主风扇的通道。
// 用户手动选过通道之后（channelLocked）就不再自动改动。
func (c *FanController) AutoChannel(readings []Reading, primary *Reading) {
	ch, ok := activeFanChannel(readings)
	if !ok && primary != nil && primary.Value > 0 {
		ch, ok = channelFromFanID(primary.ID)
	}
	if !ok {
		// 没有在转的风扇时保持当前通道不变。
		// 风扇偶尔停一下（低负载）就把控制目标切走，很容易把风扇留在错误通道上。
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.channelLocked {
		return
	}
	if c.channel == ch {
		return
	}
	if c.backend == nil || c.backend.Info().Channels <= ch {
		return
	}
	if c.mode != FanModeAuto {
		_ = c.backend.Restore(c.channel)
	}
	c.channel = ch
	c.appliedDuty = 0
}

// targetDutyLocked 计算当前模式下的目标占空比。调用方必须已持有锁。
//
// 返回 false 表示暂时算不出来（例如曲线模式但还没有温度数据）。
func (c *FanController) targetDutyLocked() (float64, bool) {
	switch c.mode {
	case FanModeManual:
		return c.duty, true
	case FanModeCurve:
		if !c.hasTemp {
			return 0, false
		}
		return c.evaluateCurve(c.lastTemp), true
	}
	return 0, false
}

// Tick 在每轮采集后调用：按当前模式计算目标占空比并写入，同时执行过温保护。
func (c *FanController) Tick(temp float64, hasTemp bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updated = time.Now()
	c.lastTemp, c.hasTemp = temp, hasTemp

	if c.backend == nil || !c.backend.Info().Available {
		return
	}

	// 正在执行临时自检测试，跳过每轮覆盖
	if c.testing {
		return
	}

	// 读回芯片里真正的占空比，这样即使被别的程序改过也能看到。
	if v, ok := c.backend.Duty(c.channel); ok {
		c.hardwareDuty = v
		c.hasDuty = true
	} else {
		c.hasDuty = false
	}

	if c.mode == FanModeAuto {
		return
	}

	// 过温保护优先级最高：无论何种模式，温度越线都强制满速。
	if hasTemp && temp >= c.safetyTemp {
		if !c.emergency || c.appliedDuty < 100 {
			c.emergency = true
			if err := c.applyLocked(100, true); err != nil {
				c.lastError = err.Error()
			}
		}
		return
	}

	// 温度回落到阈值以下，解除紧急状态并恢复设定的目标。
	if c.emergency && (!hasTemp || temp < c.safetyTemp-5) {
		c.emergency = false
	}

	// Watchdog 检测：
	// 针对联想/NEC OEM BIOS 后台定期清空手动模式或改写占空比的情况，
	// 若检测到芯片实际状态偏离预期，强制重新应用接管。
	forceReapply := false
	if mgr, ok := c.backend.(interface{ Managed(int) bool }); ok {
		if !mgr.Managed(c.channel) {
			forceReapply = true
		}
	}

	switch c.mode {
	case FanModeManual:
		target := c.duty
		if c.emergency {
			target = 100
		}
		// 若芯片读回占空比偏离设定值超过 5%，说明被 BIOS 篡改，需强制夺回
		if c.hasDuty && c.appliedDuty > 0 && math.Abs(c.hardwareDuty-target) > 5 {
			forceReapply = true
		}
		_ = c.applyLocked(target, forceReapply)
	case FanModeCurve:
		if !hasTemp {
			return
		}
		target := c.evaluateCurve(temp)
		if c.emergency {
			target = 100
		}
		if c.hasDuty && c.appliedDuty > 0 && math.Abs(c.hardwareDuty-target) > 5 {
			forceReapply = true
		}
		_ = c.applyLocked(target, forceReapply)
	}
}

// evaluateCurve 根据温度算出目标占空比（支持回差防抖）。
func (c *FanController) evaluateCurve(temp float64) float64 {
	pts := c.curve.Points
	if len(pts) == 0 {
		return 100
	}
	// 曲线点按温度排序（允许外部传入乱序）。
	sorted := make([]FanCurvePoint, len(pts))
	copy(sorted, pts)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Temp < sorted[j-1].Temp; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}

	calcDuty := func(t float64) float64 {
		if t <= sorted[0].Temp {
			return sorted[0].Duty
		}
		last := sorted[len(sorted)-1]
		if t >= last.Temp {
			return last.Duty
		}
		for i := 1; i < len(sorted); i++ {
			lo, hi := sorted[i-1], sorted[i]
			if t <= hi.Temp {
				span := hi.Temp - lo.Temp
				if span <= 0 {
					return hi.Duty
				}
				ratio := (t - lo.Temp) / span
				return lo.Duty + ratio*(hi.Duty-lo.Duty)
			}
		}
		return last.Duty
	}

	newDuty := calcDuty(temp)
	hyst := c.curve.Hysteresis
	if hyst <= 0 {
		hyst = 3.0
	}

	// 回差逻辑：
	// 温度下降时，降温幅度未达到 Hysteresis 且计算目标占空比低于上次时，
	// 继续保持上次占空比，避免风扇在临界温度来回升降速产生噪音。
	if c.hasCurveDuty {
		if temp < c.lastEvalTemp && (c.lastEvalTemp-temp) < hyst && newDuty < c.lastCurveDuty {
			return c.lastCurveDuty
		}
	}

	c.lastCurveDuty = newDuty
	c.lastEvalTemp = temp
	c.hasCurveDuty = true
	return newDuty
}

// applyLocked 把目标占空比写进硬件。调用方必须已持有锁。
//
// force 为真时即使变化很小也写入；否则只在变化超过 1% 时才写，
// 减少对硬件总线的打扰。
func (c *FanController) applyLocked(percent float64, force bool) error {
	if c.backend == nil {
		return fmt.Errorf("调速后端不可用")
	}
	if !force && math.Abs(percent-c.appliedDuty) < 1 {
		return nil
	}
	if err := c.backend.SetManual(c.channel, percent); err != nil {
		c.lastError = err.Error()
		return err
	}
	c.appliedDuty = percent
	c.lastError = ""
	return nil
}

// TestChannel 临时接管指定通道进行响应测试（持续 duration，随后自动恢复原模式）。
func (c *FanController) TestChannel(channel int, duty float64, duration time.Duration) error {
	if !c.Available() {
		return fmt.Errorf("调速后端不可用")
	}
	channels := c.backend.Info().Channels
	if channel < 0 || channel >= channels {
		return fmt.Errorf("通道号超出范围（0-%d）", channels-1)
	}
	if duty < minSafeDutyPercent || duty > 100 {
		duty = 50
	}
	if duration <= 0 || duration > 30*time.Second {
		duration = 6 * time.Second
	}

	c.mu.Lock()
	origMode := c.mode
	origDuty := c.duty
	origChan := c.channel
	c.testing = true
	c.mu.Unlock()

	// 立即接管测试
	if err := c.backend.SetManual(channel, duty); err != nil {
		c.mu.Lock()
		c.testing = false
		c.mu.Unlock()
		return err
	}

	go func() {
		time.Sleep(duration)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.testing = false
		if origMode == FanModeAuto {
			_ = c.backend.Restore(channel)
			c.appliedDuty = 0
		} else {
			if origChan != channel {
				_ = c.backend.Restore(channel)
			}
			_ = c.applyLocked(origDuty, true)
		}
	}()

	return nil
}

// Release 交还硬件控制。程序退出前调用，避免风扇卡在手动模式。
//
// 注意：这里只改内存状态，不落盘。落盘的是"用户想要什么"，
// 而不是"程序退出时是什么"——否则重启后用户设的转速就丢了。
func (c *FanController) Release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.backend == nil || c.mode == FanModeAuto {
		return
	}
	if err := c.backend.Restore(c.channel); err != nil {
		log.Printf("[fan] 交还硬件控制失败: %v", err)
	}
	c.mode = FanModeAuto
	c.appliedDuty = 0
}

// backendDetail 返回后端不可用的原因描述。
func (c *FanController) backendDetail() string {
	if c.backend == nil {
		return "当前系统不支持风扇调速"
	}
	info := c.backend.Info()
	if info.Detail != "" {
		return info.Detail
	}
	return "未检测到可控风扇的 Super I/O 芯片"
}

// describeMode 把模式翻译成中文，用于日志。
func describeMode(m FanMode) string {
	switch m {
	case FanModeAuto:
		return "自动（交还 BIOS）"
	case FanModeManual:
		return "手动固定转速"
	case FanModeCurve:
		return "温度曲线"
	default:
		return strings.TrimSpace(string(m))
	}
}
