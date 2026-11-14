//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/StackExchange/wmi"
)

// ------------------------------------------------------------------
// ACPI / WMI 数据源（兜底，不需要任何驱动）
// ------------------------------------------------------------------

// acpiSource 通过 Windows 自带的 WMI 读取 ACPI 热区与风扇信息。
// 精度不如 LHM，很多机器上取不到真实转速，但胜在零依赖。
type acpiSource struct{}

func (s *acpiSource) name() string { return "acpi-wmi" }

func (s *acpiSource) collect(_ context.Context) ([]Reading, error) {
	var out []Reading
	var errs []string

	// ACPI 热区温度：CurrentTemperature 的单位是 0.1 开尔文
	type thermalZone struct {
		InstanceName       string
		CurrentTemperature uint32
	}
	var zones []thermalZone
	if err := wmi.QueryNamespace(
		`SELECT InstanceName, CurrentTemperature FROM MSAcpi_ThermalZoneTemperature`,
		&zones, "root\\WMI"); err != nil {
		errs = append(errs, "MSAcpi_ThermalZoneTemperature: "+err.Error())
	} else {
		for _, z := range zones {
			kelvin := float64(z.CurrentTemperature) / 10
			if kelvin <= 0 {
				continue
			}
			celsius := kelvin - 273.15
			out = append(out, Reading{
				ID:       "acpi/thermal/" + z.InstanceName,
				Name:     "ACPI 热区 " + z.InstanceName,
				Category: catTemperature,
				Hardware: "ACPI",
				Value:    celsius,
				Unit:     "°C",
				Source:   "acpi-wmi",
			})
		}
	}

	// ACPI 风扇：Speed 单位是 RPM
	type acpiFan struct {
		InstanceName  string
		Speed         uint32
		ActiveCooling bool
	}
	var fans []acpiFan
	if err := wmi.QueryNamespace(
		`SELECT InstanceName, Speed, ActiveCooling FROM MSAcpi_Fan`,
		&fans, "root\\WMI"); err != nil {
		errs = append(errs, "MSAcpi_Fan: "+err.Error())
	} else {
		for _, f := range fans {
			if f.Speed == 0 || f.Speed == 0xFFFFFFFF {
				continue
			}
			out = append(out, Reading{
				ID:       "acpi/fan/" + f.InstanceName,
				Name:     "ACPI 风扇 " + f.InstanceName,
				Category: catFan,
				Hardware: "ACPI",
				Value:    float64(f.Speed),
				Unit:     "RPM",
				Source:   "acpi-wmi",
			})
		}
	}

	// Win32_Fan 的 DesiredSpeed 常常是 0，只有非 0 时才有参考价值
	type win32Fan struct {
		Name         string
		DesiredSpeed uint32
	}
	var wFans []win32Fan
	if err := wmi.QueryNamespace(
		`SELECT Name, DesiredSpeed FROM Win32_Fan`,
		&wFans, "root\\cimv2"); err != nil {
		errs = append(errs, "Win32_Fan: "+err.Error())
	} else {
		for _, f := range wFans {
			if f.DesiredSpeed == 0 {
				continue
			}
			out = append(out, Reading{
				ID:       "win32/fan/" + f.Name,
				Name:     f.Name,
				Category: catFan,
				Hardware: "系统",
				Value:    float64(f.DesiredSpeed),
				Unit:     "RPM",
				Source:   "acpi-wmi",
			})
		}
	}

	if len(out) == 0 {
		if len(errs) > 0 {
			return nil, fmt.Errorf("没有可用读数 (%s)", strings.Join(errs, "; "))
		}
		return nil, fmt.Errorf("没有可用读数")
	}
	return out, nil
}

// ------------------------------------------------------------------
// nvidia-smi 数据源（有独显时补充 GPU 温度与风扇）
// ------------------------------------------------------------------

// nvidiaSource 调用 nvidia-smi 查询 GPU 温度与风扇百分比。
type nvidiaSource struct {
	exePath string
}

func (s *nvidiaSource) name() string { return "nvidia-smi" }

func (s *nvidiaSource) collect(ctx context.Context) ([]Reading, error) {
	exe := s.exePath
	if exe == "" {
		p, err := exec.LookPath("nvidia-smi")
		if err != nil {
			return nil, fmt.Errorf("未找到 nvidia-smi")
		}
		exe = p
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe,
		"--query-gpu=name,temperature.gpu,fan.speed",
		"--format=csv,noheader,nounits")
	outBytes, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var out []Reading
	for _, line := range strings.Split(string(outBytes), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		temp, err1 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		fan, err2 := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
		if err1 == nil {
			out = append(out, Reading{
				ID:       "nvidia/temperature/" + name,
				Name:     name + " 温度",
				Category: catTemperature,
				Hardware: name,
				Value:    temp,
				Unit:     "°C",
				Source:   "nvidia-smi",
			})
		}
		if err2 == nil {
			out = append(out, Reading{
				ID:       "nvidia/fan/" + name,
				Name:     name + " 风扇",
				Category: catFan,
				Hardware: name,
				Value:    fan,
				Unit:     "%",
				Source:   "nvidia-smi",
			})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("没有可用读数")
	}
	return out, nil
}

// ------------------------------------------------------------------
// 拉起 LibreHardwareMonitor（可选自愈）
// ------------------------------------------------------------------

// lhmLauncher 负责在 LibreHardwareMonitor 未运行时把它拉起来。
type lhmLauncher struct {
	exePath string
	logf    func(string, ...any)
}

// ensure 在本地探测 LHM 的 HTTP 接口，若无响应则启动对应 exe。
// 为避免与计划任务等其它启动器重复拉起，启动前会先确认没有同名进程。
func (l *lhmLauncher) ensure(ctx context.Context, url string) {
	if l.exePath == "" {
		return
	}
	if lhmReachable(ctx, url) {
		return
	}

	// 先等一会儿，给计划任务等其它启动器留出时间
	for i := 0; i < 15; i++ {
		time.Sleep(time.Second)
		if lhmReachable(ctx, url) {
			return
		}
	}

	if lhmProcessRunning() {
		l.logf("LibreHardwareMonitor 进程已在运行但接口未就绪，跳过重复启动")
		return
	}

	l.logf("LibreHardwareMonitor 未响应，正在启动: %s", l.exePath)
	cmd := exec.Command(l.exePath)
	cmd.Dir = exeDir(l.exePath)
	if err := cmd.Start(); err != nil {
		l.logf("启动 LibreHardwareMonitor 失败: %v", err)
		return
	}
	go func() {
		for i := 0; i < 30; i++ {
			time.Sleep(time.Second)
			if lhmReachable(ctx, url) {
				l.logf("LibreHardwareMonitor 已就绪")
				return
			}
		}
		l.logf("等待 LibreHardwareMonitor 就绪超时")
	}()
}

// lhmProcessRunning 检查系统里是否已有 LibreHardwareMonitor 进程。
func lhmProcessRunning() bool {
	out, err := exec.Command("tasklist", "/NH", "/FI", "IMAGENAME eq LibreHardwareMonitor.exe").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "LibreHardwareMonitor.exe")
}

// lhmReachable 探测 LHM HTTP 接口是否可用。
func lhmReachable(ctx context.Context, url string) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/data.json", nil)
	if err != nil {
		return false
	}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// exeDir 返回可执行文件所在目录，失败时返回空串。
func exeDir(path string) string {
	dir := filepath.Dir(path)
	if dir == "." {
		return ""
	}
	return dir
}
