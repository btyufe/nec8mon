package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// 状态文件名。放在可执行文件同目录：
// 安装后是 C:\nec8mon\，卸载时整个目录一起删掉，不会留垃圾。
const (
	fanStateFileName        = "fan-state.json"
	fanRestorePointFileName = "fan-restore-point.json"
)

// fanState 是持久化的风扇控制设置。
//
// 之所以要存盘：调速设定一旦丢在内存里，服务重启或机器重启后就回到
// BIOS 默认曲线了。用户冬天手动降下来的转速，本该一直保持到他自己改。
type fanState struct {
	// Mode 是最后一次选择的控制模式。
	Mode FanMode `json:"mode"`
	// Channel 是最后一次控制的通道号。
	Channel int `json:"channel"`
	// ChannelLocked 表示通道是用户手动指定的。
	// 为假时采集循环仍然可以自动跟随"正在转"的那个通道。
	ChannelLocked bool `json:"channelLocked"`
	// Duty 是手动模式的目标占空比。
	Duty float64 `json:"duty"`
	// Curve 是温度曲线模式的曲线定义。
	Curve FanCurve `json:"curve"`
	// SafetyTemp 是过温保护阈值。
	SafetyTemp float64 `json:"safetyTemp"`
	// SavedAt 记录最后一次保存时间，方便排查。
	SavedAt time.Time `json:"savedAt"`
}

// fanRestorePoint 记录"接管某些通道之前，芯片里的原始寄存器值"。
//
// 正常退出时程序会调用 Restore 把这些值写回去；只有进程被强杀或崩溃时
// 才会来不及还原，芯片就停留在手动模式上——那样即便 CPU 过热也没有程序
// 去提转速。把还原点存下来，下次启动时就能补上这一步。
type fanRestorePoint struct {
	// Entries 按通道记录接管前的原始值。
	Entries []fanRestoreEntry `json:"entries"`
	// SavedAt 记录保存时间。
	SavedAt time.Time `json:"savedAt"`
}

// fanRestoreEntry 是单个通道的还原信息。
type fanRestoreEntry struct {
	// Channel 是被接管的通道号。
	Channel int `json:"channel"`
	// Mode 是接管前的模式寄存器值。
	// EC 风格的芯片（共享寄存器）只记录本通道对应的那一位。
	Mode byte `json:"mode"`
	// Duty 是接管前的 PWM 命令寄存器值。
	Duty byte `json:"duty"`
}

// stateFilePath 返回状态文件在可执行文件同目录下的完整路径。
//
// 取不到可执行文件路径时退回到当前目录，不会因此崩溃。
func stateFilePath(name string) string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return name
	}
	return filepath.Join(filepath.Dir(exe), name)
}

// fanStatePath 返回风扇设置文件的路径。
func fanStatePath() string { return stateFilePath(fanStateFileName) }

// fanRestorePointPath 返回还原点文件的路径。
func fanRestorePointPath() string { return stateFilePath(fanRestorePointFileName) }

// writeJSONAtomic 先写临时文件再改名，避免断电时留下半截文件。
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// loadFanState 读取已保存的风扇设置。
//
// 文件不存在时返回 ok=false（属于正常情况，不是错误）。
func loadFanState(path string) (fanState, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fanState{}, false, nil
		}
		return fanState{}, false, err
	}
	var st fanState
	if err := json.Unmarshal(data, &st); err != nil {
		return fanState{}, false, err
	}
	return st, true, nil
}

// saveFanState 保存风扇设置。失败只返回错误，由调用方决定要不要记日志。
func saveFanState(path string, st fanState) error {
	st.SavedAt = time.Now()
	return writeJSONAtomic(path, st)
}

// loadFanRestorePoint 读取还原点。
//
// 文件不存在时返回 ok=false，表示没有异常退出遗留。
func loadFanRestorePoint(path string) (fanRestorePoint, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fanRestorePoint{}, false, nil
		}
		return fanRestorePoint{}, false, err
	}
	var rp fanRestorePoint
	if err := json.Unmarshal(data, &rp); err != nil {
		return fanRestorePoint{}, false, err
	}
	return rp, true, nil
}

// saveFanRestorePoint 保存还原点。
func saveFanRestorePoint(path string, rp fanRestorePoint) error {
	rp.SavedAt = time.Now()
	return writeJSONAtomic(path, rp)
}

// addFanRestoreEntry 把一个通道的还原信息写进还原点文件。
//
// 同一个通道重复记录时以新的为准（接管前只应有一份原始值）。
func addFanRestoreEntry(path string, e fanRestoreEntry) error {
	rp, _, err := loadFanRestorePoint(path)
	if err != nil {
		// 文件损坏就当没有旧记录，重新建一份。
		rp = fanRestorePoint{}
	}
	replaced := false
	for i := range rp.Entries {
		if rp.Entries[i].Channel == e.Channel {
			rp.Entries[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		rp.Entries = append(rp.Entries, e)
	}
	return saveFanRestorePoint(path, rp)
}

// removeFanRestoreEntry 从还原点文件里移除一个通道。
//
// 通道被正常交还硬件后就不需要再还原了，移除掉可以避免下次启动
// 多做一次无谓的寄存器写入。
func removeFanRestoreEntry(path string, channel int) error {
	rp, ok, err := loadFanRestorePoint(path)
	if err != nil || !ok {
		return nil
	}
	kept := rp.Entries[:0]
	for _, e := range rp.Entries {
		if e.Channel != channel {
			kept = append(kept, e)
		}
	}
	rp.Entries = kept
	if len(rp.Entries) == 0 {
		return clearFanRestorePoint(path)
	}
	return saveFanRestorePoint(path, rp)
}

// clearFanRestorePoint 删除还原点文件。
//
// 文件本来就不存在时不算错误——那正是"没有残留"的正常状态。
func clearFanRestorePoint(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
