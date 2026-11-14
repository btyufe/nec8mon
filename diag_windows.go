//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

// diagOut 是诊断报告的输出目标。
var diagOut io.Writer = os.Stdout

// diagSuperIO 保存本轮诊断识别出的芯片，供调速状态小节使用。
var diagSuperIO *SuperIO

// diagPrintf / diagPrintln 把报告写到 diagOut。
func diagPrintf(format string, a ...any) { fmt.Fprintf(diagOut, format, a...) }
func diagPrintln(a ...any)               { fmt.Fprintln(diagOut, a...) }

// runDiag 生成现场诊断报告。
//
// 用途：在目标机器上排查"温度/转速读不到"、"Super I/O 探测失败"这类问题。
// 报告同时打印到控制台并保存成 UTF-8 文件（nec8mon-diag.txt），
// 方便直接贴回对话或作为附件回传。
func runDiag(lhmURL string) int {
	var buf bytes.Buffer
	diagOut = &buf
	code := diagReport(lhmURL)
	diagOut = os.Stdout

	text := buf.String()
	consoleWrite(os.Stdout, text)

	if path, err := saveDiagReport(text); err == nil {
		consoleWrite(os.Stdout, "\n诊断报告已保存到: "+path+"\n")
	} else {
		consoleWrite(os.Stdout, "\n(诊断报告保存失败: "+err.Error()+")\n")
	}
	return code
}

// saveDiagReport 把诊断报告写成 UTF-8(带 BOM) 文件。
//
// 优先写在与可执行文件同级的目录；没有写权限时退回当前工作目录。
func saveDiagReport(text string) (string, error) {
	data := append([]byte{0xEF, 0xBB, 0xBF}, []byte(text)...)

	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "nec8mon-diag.txt"))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "nec8mon-diag.txt"))
	}

	var lastErr error
	for _, path := range candidates {
		if err := os.WriteFile(path, data, 0o644); err == nil {
			return path, nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("没有可写的路径")
	}
	return "", lastErr
}

// diagReport 生成诊断报告正文。
func diagReport(lhmURL string) int {
	diagPrintln("========================================================")
	diagPrintln(" nec8mon 现场诊断")
	diagPrintf(" 时间: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	diagPrintln("========================================================")

	section("1. 基本信息")
	diagPrintf("  计算机名    : %s\n", hostname())
	diagPrintf("  CPU         : %s\n", cpuName())
	diagPrintf("  操作系统    : %s\n", osVersion())
	diagPrintf("  运行账户    : %s\n", currentUser())

	section("2. PawnIO 驱动")
	ver, installed := pawnIOVersion()
	if installed {
		diagPrintf("  驱动版本    : %s\n", ver)
	} else {
		diagPrintln("  驱动版本    : 未检测到（注册表里没有 PawnIO 的卸载项）")
		diagPrintln("  → 请先运行 install.ps1 安装 PawnIO 驱动")
	}

	pawn, err := OpenPawnIO()
	if err != nil {
		diagPrintf("  打开设备    : 失败 —— %v\n", err)
		return reportLHM(lhmURL)
	}
	diagPrintln("  打开设备    : 成功")
	defer pawn.Close()

	// PawnIO 驱动限制：一个设备句柄只能加载一个模块（句柄的 FsContext 非空时
	// 再加载会返回 STATUS_ALREADY_INITIALIZED）。Super I/O 与 Intel MSR
	// 因此必须各用一个句柄，下面的第 5 节就是这么做的。
	if err := pawn.LoadModule(lpcIOModule); err != nil {
		diagPrintf("  加载 LpcIO  : 失败 —— %v\n", err)
	} else {
		diagPrintln("  加载 LpcIO  : 成功")
	}
	diagPrintln("  加载 IntelMSR: 单独开一个句柄加载（见第 5 节）")

	section("3. Super I/O 探测过程")
	diagPrintln("  （依次尝试 0x2E / 0x4E 两个配置端口与两种解锁序列）")
	results := ProbeSuperIO(pawn)
	for _, r := range results {
		diagPrintf("  · %s\n", r.String())
	}

	section("4. 直接读取硬件传感器")
	sio, err := detectSuperIOFrom(pawn, results)
	if err != nil && sio == nil {
		diagPrintf("  探测失败    : %v\n", err)
	} else {
		if err != nil {
			diagPrintf("  提示        : %v\n", err)
		}
		diagPrintf("  芯片        : %s\n", sio.ChipName())
		diagPrintf("  运行时基址  : 0x%04X\n", sio.BasePort())
		diagSuperIO = sio

		// 直接验证驱动内部的端口白名单：驱动只放行"基址所在 8 字节块"内的
		// 端口，能读通就说明白名单已经重新填好。
		probePort := sio.BasePort() + ecIndexRegisterOff
		if v, perr := sio.pawn.ReadPortByte(probePort); perr != nil {
			diagPrintf("  端口白名单  : 异常 —— %v\n", perr)
			diagPrintln("  → 驱动拒绝访问该端口，通常是扫描基址的步骤被后续操作清空了")
		} else {
			diagPrintf("  端口白名单  : 正常（读 0x%04X 得到 0x%02X）\n", probePort, v)
		}

		if temps, err := sio.Temperatures(); err != nil {
			diagPrintf("  温度读取    : 失败 —— %v\n", err)
		} else {
			diagPrintln("  温度读取    : 成功")
			for _, t := range temps {
				diagPrintf("      %s %6.1f °C\n", padRight(t.Name, 16), t.Value)
			}
		}

		if fans, err := sio.Fans(); err != nil {
			diagPrintf("  风扇读取    : 失败 —— %v\n", err)
		} else {
			diagPrintln("  风扇读取    : 成功")
			for _, f := range fans {
				duty := "  "
				if f.HasDuty {
					duty = fmt.Sprintf("%3.0f%%", f.Duty)
				}
				diagPrintf("      %s %6.0f RPM  占空比 %s\n", padRight(f.Name, 30), f.RPM, duty)
			}
			// 直接告诉用户该调哪个通道：名字不可靠，转速才可靠。
			best := -1
			bestRPM := 0.0
			for _, f := range fans {
				if f.Channel >= 0 && f.RPM > bestRPM {
					bestRPM, best = f.RPM, f.Channel
				}
			}
			if best >= 0 {
				diagPrintf("  正在转的通道: %d（%.0f RPM）—— 调速请选这个通道\n", best, bestRPM)
			} else {
				diagPrintln("  正在转的通道: 没有检测到在转的风扇")
			}
			diagPrintln("  → 转速为 0 的通道通常是没接风扇；只有带占空比的通道才能调速")
		}
	}

	section("5. 风扇调速状态与设置")
	reportFanState()

	section("6. Intel MSR 温度")
	if msr, err := OpenPawnIO(); err == nil {
		defer msr.Close()
		if err := msr.LoadModule(intelMSRModule); err != nil {
			diagPrintf("  加载模块失败: %v\n", err)
		} else {
			if target, err := msr.ReadMSR(msrTemperatureTarget); err == nil {
				tjMax := (target >> 16) & 0xFF
				diagPrintf("  TJMax       : %d °C\n", tjMax)
				if status, err := msr.ReadMSR(msrPackageThermStatus); err == nil {
					if uint32(status)&0x80000000 != 0 {
						delta := float64((uint32(status) & 0x007F0000) >> 16)
						diagPrintf("  封装温度    : %.1f °C\n", float64(tjMax)-delta)
					} else {
						diagPrintln("  封装温度    : 读取无效（IA32_PACKAGE_THERM_STATUS bit31 未置位）")
					}
				} else {
					diagPrintf("  封装温度    : 读取失败 —— %v\n", err)
				}
				if status, err := msr.ReadMSR(msrThermStatus); err == nil {
					if uint32(status)&0x80000000 != 0 {
						delta := float64((uint32(status) & 0x007F0000) >> 16)
						diagPrintf("  核心温度    : %.1f °C\n", float64(tjMax)-delta)
					} else {
						diagPrintln("  核心温度    : 读取无效")
					}
				} else {
					diagPrintf("  核心温度    : 读取失败 —— %v\n", err)
				}
			} else {
				diagPrintf("  读取 TJMax  : 失败 —— %v\n", err)
				diagPrintln("  → 该机可能不是 Intel CPU，或 CPU 不支持这些 MSR")
			}
		}
	}

	section("7. ACPI / WMI 兜底数据源")
	acpi := &acpiSource{}
	readings, err := acpi.collect(context.Background())
	if err != nil {
		diagPrintf("  读取失败    : %v\n", err)
	} else {
		diagPrintf("  读到 %d 条\n", len(readings))
		for _, r := range readings {
			diagPrintf("      %s %10s  [%s]\n", padRight(r.Name, 28), formatValue(r.Value, r.Unit), r.Source)
		}
	}

	return reportLHM(lhmURL)
}

// reportFanState 输出风扇调速的当前状态与持久化设置。
//
// 现场排查"调速有没有生效"时，这一节能直接看出：
// 服务在用哪个通道、占空比是多少、设置有没有存盘。
func reportFanState() {
	// 芯片当前处于手动还是自动模式。
	// 注意这里读的是运行中的服务状态，诊断程序自己另开了一个句柄，
	// 读到的模式位反映的是芯片的真实状态。
	if sio := diagSuperIO; sio != nil {
		if mode, err := sio.readModeBits(); err == nil {
			manual := []string{}
			for ch := 0; ch < sio.layout.channels; ch++ {
				if mode&(1<<uint(ch)) != 0 {
					manual = append(manual, fmt.Sprintf("%d", ch))
				}
			}
			if len(manual) > 0 {
				diagPrintf("  芯片手动通道: %s（这些通道正被手动模式接管）\n", strings.Join(manual, ", "))
			} else {
				diagPrintln("  芯片手动通道: 无（当前由 BIOS 自动控制）")
			}
		}
	}

	// 磁盘上保存的设置。
	statePath := fanStatePath()
	st, ok, err := loadFanState(statePath)
	switch {
	case err != nil:
		diagPrintf("  设置文件    : 读取失败 —— %v\n", err)
	case !ok:
		diagPrintf("  设置文件    : %s（尚未生成）\n", statePath)
	default:
		diagPrintf("  设置文件    : %s\n", statePath)
		diagPrintf("  保存的模式  : %s\n", describeMode(st.Mode))
		diagPrintf("  保存的通道  : %d%s\n", st.Channel,
			map[bool]string{true: "（已锁定）", false: "（自动跟随）"}[st.ChannelLocked])
		diagPrintf("  保存的占空比: %.0f%%\n", st.Duty)
		diagPrintf("  保存的过温线: %.0f °C\n", st.SafetyTemp)
		diagPrintf("  保存时间    : %s\n", st.SavedAt.Format("2006-01-02 15:04:05"))
	}

	// 异常退出残留的还原点。
	rpPath := fanRestorePointPath()
	rp, ok, err := loadFanRestorePoint(rpPath)
	switch {
	case err != nil:
		diagPrintf("  还原点      : 读取失败 —— %v\n", err)
	case !ok || len(rp.Entries) == 0:
		diagPrintln("  还原点      : 无（上次退出正常）")
	default:
		diagPrintf("  还原点      : %s\n", rpPath)
		for _, e := range rp.Entries {
			diagPrintf("      通道 %d：原始模式 0x%02X，原始占空比 0x%02X\n", e.Channel, e.Mode, e.Duty)
		}
		diagPrintln("  → 上次进程异常退出，风扇可能仍停在手动模式；下次启动会自动还原")
	}
}

// reportLHM 检查 LibreHardwareMonitor 的 HTTP 接口并输出摘要。
func reportLHM(lhmURL string) int {
	section("8. LibreHardwareMonitor 接口")
	if lhmURL == "" {
		lhmURL = "http://127.0.0.1:8085"
	}
	url := strings.TrimRight(lhmURL, "/") + "/data.json"

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		diagPrintf("  请求 %s 失败: %v\n", url, err)
		diagPrintln("  → LibreHardwareMonitor 未运行或端口不通（本程序已不依赖它）")
		return 0
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		diagPrintf("  读取响应失败: %v\n", err)
		return 0
	}
	diagPrintf("  HTTP 状态   : %s\n", resp.Status)
	diagPrintf("  响应长度    : %d 字节\n", len(body))

	var root struct {
		Text     string            `json:"Text"`
		Children []json.RawMessage `json:"Children"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		diagPrintf("  JSON 解析失败: %v\n", err)
		diagPrintf("  原始内容前 500 字节: %s\n", truncate(string(body), 500))
		return 0
	}
	diagPrintf("  根节点      : %s\n", root.Text)
	diagPrintf("  子节点数    : %d\n", len(root.Children))
	if len(root.Children) == 0 {
		diagPrintln("  → LHM 没有枚举到任何硬件（PawnIO 未生效或无头启动失败）")
		diagPrintf("  原始内容前 500 字节: %s\n", truncate(string(body), 500))
	}
	return 0
}

// section 打印一个小节标题。
func section(title string) {
	diagPrintln()
	diagPrintln("--------------------------------------------------------")
	diagPrintln(" " + title)
	diagPrintln("--------------------------------------------------------")
}

// truncate 截断字符串并补上省略号。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + " …"
}

// pawnIOVersion 从注册表读取 PawnIO 驱动版本。
func pawnIOVersion() (string, bool) {
	for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		for _, view := range []uint32{registry.WOW64_64KEY, registry.WOW64_32KEY} {
			key, err := registry.OpenKey(root, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\PawnIO`, registry.QUERY_VALUE|view)
			if err != nil {
				continue
			}
			v, _, err := key.GetStringValue("DisplayVersion")
			key.Close()
			if err == nil && v != "" {
				return v, true
			}
		}
	}
	return "", false
}

// cpuName 从注册表读取 CPU 型号。
func cpuName() string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE)
	if err != nil {
		return "未知"
	}
	defer key.Close()
	name, _, err := key.GetStringValue("ProcessorNameString")
	if err != nil {
		return "未知"
	}
	return strings.TrimSpace(name)
}

// osVersion 读取操作系统版本。
func osVersion() string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return "未知"
	}
	defer key.Close()
	product, _, _ := key.GetStringValue("ProductName")
	display, _, _ := key.GetStringValue("DisplayVersion")
	build, _, _ := key.GetStringValue("CurrentBuildNumber")
	product = strings.TrimSpace(product)
	if display != "" {
		return fmt.Sprintf("%s %s (内部版本 %s)", product, display, build)
	}
	return fmt.Sprintf("%s (内部版本 %s)", product, build)
}

// currentUser 返回当前登录的账户名。
func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	if name := os.Getenv("USERNAME"); name != "" {
		return name
	}
	return "未知"
}
