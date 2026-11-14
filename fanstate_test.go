package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFanStateRoundTrip 设置写入磁盘再读回来，内容必须一致。
func TestFanStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fan-state.json")

	curve := FanCurve{
		Points: []FanCurvePoint{
			{Temp: 30, Duty: 20},
			{Temp: 80, Duty: 90},
		},
		Hysteresis: 3,
	}
	want := fanState{
		Mode:          FanModeCurve,
		Channel:       3,
		ChannelLocked: true,
		Duty:          42,
		Curve:         curve,
		SafetyTemp:    72,
	}
	if err := saveFanState(path, want); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	got, ok, err := loadFanState(path)
	if err != nil || !ok {
		t.Fatalf("读取失败: ok=%v err=%v", ok, err)
	}
	if got.Mode != want.Mode || got.Channel != want.Channel ||
		got.ChannelLocked != want.ChannelLocked || got.Duty != want.Duty ||
		got.SafetyTemp != want.SafetyTemp {
		t.Errorf("读回的内容不一致: %+v", got)
	}
	if len(got.Curve.Points) != len(curve.Points) {
		t.Fatalf("曲线点数 = %d，期望 %d", len(got.Curve.Points), len(curve.Points))
	}
	for i, p := range curve.Points {
		if got.Curve.Points[i] != p {
			t.Errorf("曲线第 %d 点 = %+v，期望 %+v", i, got.Curve.Points[i], p)
		}
	}
}

// TestFanStateMissingFile 文件不存在不算错误。
func TestFanStateMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	_, ok, err := loadFanState(path)
	if err != nil {
		t.Fatalf("文件不存在不该报错: %v", err)
	}
	if ok {
		t.Error("文件不存在时 ok 应当为 false")
	}
}

// TestRestoreStateAppliesToBackend 恢复设置后，手动模式要真的写进硬件。
func TestRestoreStateAppliesToBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fan-state.json")
	backend := &fakeFanBackend{available: true, channels: 8}
	c := NewFanController(backend)

	st := fanState{
		Mode:          FanModeManual,
		Channel:       2,
		ChannelLocked: true,
		Duty:          35,
		SafetyTemp:    70,
	}
	if err := saveFanState(path, st); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	if err := c.RestoreState(path); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	s := c.State()
	if s.Mode != FanModeManual {
		t.Errorf("模式 = %v，期望 manual", s.Mode)
	}
	if s.Channel != 2 {
		t.Errorf("通道 = %d，期望 2", s.Channel)
	}
	if s.Duty != 35 {
		t.Errorf("占空比 = %v，期望 35", s.Duty)
	}
	if s.SafetyTemp != 70 {
		t.Errorf("过温阈值 = %v，期望 70", s.SafetyTemp)
	}
	if backend.duty != 35 {
		t.Errorf("硬件收到的占空比 = %v，期望 35", backend.duty)
	}
}

// TestRestoreStateIgnoresBadValues 存档里有非法值时不能把控制器带坏。
func TestRestoreStateIgnoresBadValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fan-state.json")
	backend := &fakeFanBackend{available: true, channels: 2}
	c := NewFanController(backend)

	// 占空比 5 低于安全下限、过温 200 超范围、通道 99 越界，都应当被丢弃。
	bad := fanState{
		Mode:       FanModeManual,
		Channel:    99,
		Duty:       5,
		SafetyTemp: 200,
	}
	if err := saveFanState(path, bad); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if err := c.RestoreState(path); err != nil {
		t.Fatalf("恢复不应报错: %v", err)
	}

	s := c.State()
	if s.Duty != 30 {
		t.Errorf("占空比应保持默认值 30，实际 %v", s.Duty)
	}
	if s.SafetyTemp != 75 {
		t.Errorf("过温阈值应保持默认值 75，实际 %v", s.SafetyTemp)
	}
	if s.Channel != 0 {
		t.Errorf("通道应保持默认值 0，实际 %d", s.Channel)
	}
}

// TestSetDutyPersists 设置变化后应当立刻落盘，
// 这样网页关掉、程序重启后设置还在。
func TestSetDutyPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fan-state.json")
	backend := &fakeFanBackend{available: true, channels: 4}
	c := NewFanController(backend)
	c.EnablePersistence(path)

	if err := c.SetMode(FanModeManual); err != nil {
		t.Fatalf("切手动失败: %v", err)
	}
	if err := c.SetDuty(28); err != nil {
		t.Fatalf("设置占空比失败: %v", err)
	}

	st, ok, err := loadFanState(path)
	if err != nil || !ok {
		t.Fatalf("设置文件没有生成: ok=%v err=%v", ok, err)
	}
	if st.Duty != 28 {
		t.Errorf("落盘的占空比 = %v，期望 28", st.Duty)
	}
	if st.Mode != FanModeManual {
		t.Errorf("落盘的模式 = %v，期望 manual", st.Mode)
	}
}

// TestSetChannelPersists 通道选择也要落盘。
func TestSetChannelPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fan-state.json")
	backend := &fakeFanBackend{available: true, channels: 8}
	c := NewFanController(backend)
	c.EnablePersistence(path)

	if err := c.SetChannel(5); err != nil {
		t.Fatalf("设置通道失败: %v", err)
	}
	st, ok, err := loadFanState(path)
	if err != nil || !ok {
		t.Fatalf("设置文件没有生成: ok=%v err=%v", ok, err)
	}
	if st.Channel != 5 {
		t.Errorf("落盘的通道 = %d，期望 5", st.Channel)
	}
	if !st.ChannelLocked {
		t.Error("手动选过通道后应当记录为已锁定")
	}
}

// TestFanRestorePointEntries 还原点文件支持多通道增删。
func TestFanRestorePointEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rp.json")

	if err := addFanRestoreEntry(path, fanRestoreEntry{Channel: 1, Mode: 0x02, Duty: 100}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := addFanRestoreEntry(path, fanRestoreEntry{Channel: 3, Mode: 0x08, Duty: 80}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	rp, ok, err := loadFanRestorePoint(path)
	if err != nil || !ok {
		t.Fatalf("读取失败: ok=%v err=%v", ok, err)
	}
	if len(rp.Entries) != 2 {
		t.Fatalf("还原点条目数 = %d，期望 2", len(rp.Entries))
	}

	// 同一通道重复写入应当替换而不是追加。
	if err := addFanRestoreEntry(path, fanRestoreEntry{Channel: 1, Mode: 0x02, Duty: 60}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	rp, _, _ = loadFanRestorePoint(path)
	if len(rp.Entries) != 2 {
		t.Fatalf("覆盖后条目数 = %d，期望 2", len(rp.Entries))
	}
	for _, e := range rp.Entries {
		if e.Channel == 1 && e.Duty != 60 {
			t.Errorf("通道 1 的占空比 = %d，期望 60", e.Duty)
		}
	}

	// 移除一个通道后还剩一个。
	if err := removeFanRestoreEntry(path, 1); err != nil {
		t.Fatalf("移除失败: %v", err)
	}
	rp, _, _ = loadFanRestorePoint(path)
	if len(rp.Entries) != 1 || rp.Entries[0].Channel != 3 {
		t.Errorf("移除后内容不对: %+v", rp.Entries)
	}

	// 全部移除后文件应当被清掉。
	if err := removeFanRestoreEntry(path, 3); err != nil {
		t.Fatalf("移除失败: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("全部移除后还原点文件应当被删除")
	}
}

// TestClearFanRestorePointMissing 清一个不存在的还原点不算错误。
func TestClearFanRestorePointMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	if err := clearFanRestorePoint(path); err != nil {
		t.Errorf("删除不存在的文件不该报错: %v", err)
	}
}
