package main

import (
	"context"
	"errors"
	"log"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// verboseLog 控制采集过程是否输出逐轮日志。
var verboseLog = false

// source 是一个可轮询的传感器数据源。
type source interface {
	// name 返回数据源的稳定标识，会出现在 API 与前端状态条里。
	name() string
	// collect 抓取一次数据。返回空切片表示该源当前没有可用数据。
	collect(ctx context.Context) ([]Reading, error)
}

// Collector 汇总所有数据源，产出统一快照，并维护一段历史用于画趋势图。
type Collector struct {
	sources []source
	fan     *FanController
	started time.Time

	mu      sync.RWMutex
	current Snapshot
	history []historyPoint
	maxHist int
}

// SetFanController 绑定风扇控制器。每轮采集后会把温度交给它，
// 由它决定要不要调整风扇转速。
func (c *Collector) SetFanController(f *FanController) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fan = f
}

// NewCollector 创建采集器。
func NewCollector(maxHistory int) *Collector {
	if maxHistory <= 0 {
		maxHistory = 600
	}
	return &Collector{
		started: time.Now(),
		maxHist: maxHistory,
		current: Snapshot{Readings: []Reading{}, Sources: []SourceStatus{}, Warnings: []string{}},
	}
}

// AddSource 追加一个数据源。
func (c *Collector) AddSource(s source) {
	c.sources = append(c.sources, s)
}

// Start 周期性采集，直到 ctx 结束。
func (c *Collector) Start(ctx context.Context, interval time.Duration) {
	c.refresh(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh(ctx)
		}
	}
}

// refresh 执行一轮采集并更新当前快照。
func (c *Collector) refresh(ctx context.Context) {
	var (
		all      = []Reading{}
		statuses = []SourceStatus{}
		warnings = []string{}
	)

	for _, s := range c.sources {
		start := time.Now()
		readings, err := s.collect(ctx)
		elapsed := time.Since(start)

		st := SourceStatus{
			Name:    s.name(),
			OK:      err == nil,
			Count:   len(readings),
			Updated: start,
		}
		switch {
		case err != nil:
			st.Detail = err.Error()
			warnings = append(warnings, s.name()+": "+err.Error())
			if verboseLog {
				log.Printf("[collect] %s 失败: %v", s.name(), err)
			}
		case len(readings) == 0:
			st.Detail = "没有可用读数"
			warnings = append(warnings, s.name()+": 没有可用读数")
		default:
			st.Detail = formatDuration(elapsed)
			all = append(all, readings...)
		}
		statuses = append(statuses, st)
	}

	sortReadings(all)

	snap := Snapshot{
		Hostname:      hostname(),
		UpdatedAt:     time.Now(),
		UptimeSeconds: time.Since(c.started).Seconds(),
		Sources:       statuses,
		Warnings:      warnings,
		Readings:      all,
	}
	ctrlChan := -1
	c.mu.RLock()
	if c.fan != nil {
		ctrlChan = c.fan.State().Channel
	}
	c.mu.RUnlock()

	snap.CPUTemperature = pickPrimaryCPU(all)
	snap.Fan = pickPrimaryFan(all, ctrlChan)
	snap.FanPercent = fanPercentReading(all, snap.Fan)

	c.mu.Lock()
	c.current = snap
	c.appendHistory(snap)
	fan := c.fan
	c.mu.Unlock()

	// 把最新的温度交给风扇控制器：曲线模式下按温度调速，
	// 同时它会执行过温保护。
	if fan != nil {
		fan.AutoChannel(all, snap.Fan)
		if snap.CPUTemperature != nil {
			fan.Tick(snap.CPUTemperature.Value, true)
		} else {
			fan.Tick(0, false)
		}
	}
}

// appendHistory 记录一个历史点。调用方必须已持有写锁。
func (c *Collector) appendHistory(snap Snapshot) {
	p := historyPoint{T: snap.UpdatedAt}
	if snap.CPUTemperature != nil {
		v := snap.CPUTemperature.Value
		p.Temp = &v
	}
	if snap.Fan != nil {
		v := snap.Fan.Value
		p.Fan = &v
	}
	c.history = append(c.history, p)
	if len(c.history) > c.maxHist {
		c.history = c.history[len(c.history)-c.maxHist:]
	}
}

// Snapshot 返回当前快照的副本。
func (c *Collector) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current
}

// History 返回历史采样点。
func (c *Collector) History() []historyPoint {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]historyPoint, len(c.history))
	copy(out, c.history)
	return out
}

// sortReadings 按 硬件 -> 类别 -> 名称 排序，让前端展示稳定。
func sortReadings(rs []Reading) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if a.Hardware != b.Hardware {
			return a.Hardware < b.Hardware
		}
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		return a.Name < b.Name
	})
}

// hostname 返回本机名，失败时退化为 "unknown"。
func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// formatDuration 把耗时格式化成人类可读的短字符串。
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return strconv.FormatFloat(float64(d.Nanoseconds())/1e6, 'f', 2, 64) + "ms"
	case d < time.Second:
		return d.Round(100 * time.Microsecond).String()
	default:
		return d.Round(time.Millisecond).String()
	}
}

// errNoSource 表示数据源不可用。
var errNoSource = errors.New("数据源不可用")
