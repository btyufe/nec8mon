package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// parseFanSetSpec 解析 "-fanset <通道>:<占空比>" 参数。
func parseFanSetSpec(spec string) (int, float64, error) {
	s := strings.TrimSpace(spec)
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("格式应为 <通道>:<占空比>，例如 1:40")
	}
	ch, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("通道号必须是整数: %q", parts[0])
	}
	duty, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("占空比必须是数字: %q", parts[1])
	}
	if ch < 0 {
		return 0, 0, fmt.Errorf("通道号不能为负")
	}
	if duty < minSafeDutyPercent || duty > 100 {
		return 0, 0, fmt.Errorf("占空比需在 %d%%-100%% 之间", int(minSafeDutyPercent))
	}
	return ch, duty, nil
}

// runFanSet 是一次性的调速自检。
//
// 现场排查"调速不起作用"时最有用：不依赖网页界面，直接在命令行里
// 验证某个通道到底能不能被接管、转速会不会跟着变。跑完自动把风扇
// 交还 BIOS，不会把机器留在手动模式。
//
// 用法：nec8mon -fanset 1:40
func runFanSet(spec string) int {
	ch, duty, err := parseFanSetSpec(spec)
	if err != nil {
		consoleWrite(os.Stdout, "参数错误: "+err.Error()+"\n")
		consoleWrite(os.Stdout, "用法: nec8mon -fanset <通道>:<占空比>   例如 -fanset 1:40\n")
		return 2
	}

	backend, sources := newPlatformBackend(true)
	if closer, ok := backend.(interface{ Close() }); ok {
		defer closer.Close()
	}
	info := backend.Info()
	if !info.Available {
		consoleWrite(os.Stdout, "调速后端不可用: "+info.Detail+"\n")
		return 1
	}
	consoleWrite(os.Stdout, fmt.Sprintf("芯片    : %s\n详情    : %s\n可控通道: %d\n\n",
		info.Chip, info.Detail, info.Channels))
	if ch >= info.Channels {
		consoleWrite(os.Stdout, fmt.Sprintf("通道 %d 超出范围 (0-%d)\n", ch, info.Channels-1))
		return 1
	}

	consoleWrite(os.Stdout, "【接管前】各通道现状（* 号是要测试的通道）\n")
	printFanSnapshot(sources, ch)

	ctl := NewFanController(backend)
	if err := ctl.SetChannel(ch); err != nil {
		consoleWrite(os.Stdout, "设置通道失败: "+err.Error()+"\n")
		return 1
	}
	if err := ctl.SetMode(FanModeManual); err != nil {
		consoleWrite(os.Stdout, "切换手动模式失败: "+err.Error()+"\n")
		return 1
	}
	if err := ctl.SetDuty(duty); err != nil {
		consoleWrite(os.Stdout, "写入占空比失败: "+err.Error()+"\n")
		return 1
	}
	consoleWrite(os.Stdout, fmt.Sprintf("\n已接管通道 %d，占空比 %.0f%%，等待风扇响应…\n", ch, duty))

	for i := 1; i <= 3; i++ {
		time.Sleep(2 * time.Second)
		rpm := fanChannelRPM(sources, ch)
		line := fmt.Sprintf("  +%ds  通道 %d 转速 %.0f RPM", i*2, ch, rpm)
		if actCh, actRPM := fastestFan(sources); actCh >= 0 && actCh != ch && actRPM > 0 {
			line += fmt.Sprintf("（通道 %d 实测 %.0f RPM）", actCh, actRPM)
		}
		if back, ok := backend.Duty(ch); ok {
			line += fmt.Sprintf("  芯片读回占空比 %.0f%%", back)
		}
		consoleWrite(os.Stdout, line+"\n")
	}

	// 交还硬件，避免把机器留在手动模式。
	_ = ctl.SetMode(FanModeAuto)
	consoleWrite(os.Stdout, "\n【已交还 BIOS】各通道现状\n")
	printFanSnapshot(sources, ch)

	if rpm := fanChannelRPM(sources, ch); rpm <= 0 {
		consoleWrite(os.Stdout, "\n提示：该通道转速仍为 0，说明这个位置没有接风扇，"+
			"或者 BIOS 把 PWM 输出固定住了。\n")
	}
	return 0
}

// fanChannelRPM 读取某个通道当前的转速，读不到返回 -1。
func fanChannelRPM(sources []source, ch int) float64 {
	ctx := context.Background()
	for _, s := range sources {
		readings, err := s.collect(ctx)
		if err != nil {
			continue
		}
		for _, r := range readings {
			if r.Category != catFan || !strings.EqualFold(r.Unit, "RPM") {
				continue
			}
			if c, ok := channelFromFanID(r.ID); ok && c == ch {
				return r.Value
			}
		}
	}
	return -1
}

// fastestFan 返回当前所有通道中转速最高的通道和转速。
func fastestFan(sources []source) (int, float64) {
	ctx := context.Background()
	bestCh := -1
	bestRPM := 0.0
	for _, s := range sources {
		readings, err := s.collect(ctx)
		if err != nil {
			continue
		}
		for _, r := range readings {
			if r.Category != catFan || !strings.EqualFold(r.Unit, "RPM") {
				continue
			}
			if c, ok := channelFromFanID(r.ID); ok {
				if r.Value > bestRPM {
					bestRPM = r.Value
					bestCh = c
				}
			}
		}
	}
	return bestCh, bestRPM
}

// printFanSnapshot 打印所有风扇通道的现状。
// highlight 为需要标记的通道号，-1 表示不标记。
func printFanSnapshot(sources []source, highlight int) {
	type row struct {
		ch      int
		name    string
		rpm     float64
		duty    float64
		hasDuty bool
	}
	var rows []row

	ctx := context.Background()
	for _, s := range sources {
		readings, err := s.collect(ctx)
		if err != nil {
			continue
		}
		for _, r := range readings {
			ch, ok := channelFromFanID(r.ID)
			if !ok {
				continue
			}
			if r.Category == catFan && strings.EqualFold(r.Unit, "RPM") {
				found := false
				for i := range rows {
					if rows[i].ch == ch {
						rows[i].rpm = r.Value
						if r.Name != "" {
							rows[i].name = r.Name
						}
						found = true
						break
					}
				}
				if !found {
					rows = append(rows, row{ch: ch, name: r.Name, rpm: r.Value})
				}
				continue
			}
			if r.Unit == "%" {
				for i := range rows {
					if rows[i].ch == ch {
						rows[i].duty = r.Value
						rows[i].hasDuty = true
					}
				}
			}
		}
	}

	if len(rows) == 0 {
		consoleWrite(os.Stdout, "没有读到任何风扇通道\n")
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ch < rows[j].ch })

	consoleWrite(os.Stdout, "   通道  名称                            转速       占空比\n")
	for _, r := range rows {
		duty := "—"
		if r.hasDuty {
			duty = fmt.Sprintf("%3.0f%%", r.duty)
		}
		mark := " "
		if r.ch == highlight {
			mark = "*"
		}
		consoleWrite(os.Stdout, fmt.Sprintf(" %s %-4d  %s %6.0f RPM  %s\n",
			mark, r.ch, padRight(r.name, 30), r.rpm, duty))
	}
}
