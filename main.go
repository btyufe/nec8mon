package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version 是程序版本号。
const version = "2.4.7"

func main() {
	var (
		addr        = flag.String("addr", ":8080", "HTTP 监听地址；默认监听所有网卡支持局域网访问，仅本机访问可传 127.0.0.1:8080")
		poll        = flag.Duration("poll", 2*time.Second, "传感器轮询间隔")
		lhmURL      = flag.String("lhm", "http://127.0.0.1:8085", "LibreHardwareMonitor 的 HTTP 地址")
		lhmExe      = flag.String("lhm-exe", "", "LibreHardwareMonitor.exe 路径；指定后未运行时自动拉起")
		historySize = flag.Int("history", 900, "保留的历史采样点数")
		asService   = flag.Bool("svc", false, "以 Windows 服务方式运行")
		showVersion = flag.Bool("version", false, "打印版本号后退出")
		verbose     = flag.Bool("v", false, "输出更详细的日志")
		openUI      = flag.Bool("open", false, "启动后用默认浏览器打开仪表盘")
		fanEnabled  = flag.Bool("fan", true, "启用风扇调速功能（需要管理员权限与 PawnIO 驱动）")
		fanDuty     = flag.Float64("fan-duty", -1, "手动模式下的初始占空比百分比（-1 表示沿用上次保存的设置）")
		fanSafety   = flag.Float64("fan-safety", 0, "过温保护阈值（摄氏度），0 表示沿用上次保存的设置")
		fanChan     = flag.Int("fan-channel", -1, "指定要调速的风扇通道（-1 表示自动跟随在转的那个）")
		fanSet      = flag.String("fanset", "", "一次性调速自检：<通道>:<占空比>，例如 -fanset 1:40；跑完自动交还 BIOS")
		useLHM      = flag.Bool("with-lhm", false, "同时采集 LibreHardwareMonitor 的数据作为补充（需另行部署 LHM）")
		diagnose    = flag.Bool("diag", false, "打印现场诊断报告后退出")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("nec8mon %s\n", version)
		return
	}

	if *diagnose {
		// 诊断报告里有大量中文，先把控制台切到 UTF-8 免得显示成乱码。
		enableUTF8Console()
		os.Exit(runDiag(*lhmURL))
	}

	if *fanSet != "" {
		// 一次性调速自检：现场排查"调速不起作用"最直接的办法。
		enableUTF8Console()
		os.Exit(runFanSet(*fanSet))
	}

	log.SetFlags(log.LstdFlags)
	log.SetPrefix("")
	log.SetOutput(newConsoleWriter())
	verboseLog = *verbose

	// 中文日志在 GBK 控制台上会显示成乱码。
	enableUTF8Console()

	collector := NewCollector(*historySize)

	// 直接从 Super I/O 芯片读取温度与风扇转速（主数据源，依赖 PawnIO 驱动）。
	// 调速后端与数据源共用同一个硬件句柄，避免重复探测芯片。
	backend, hwSources := newPlatformBackend(*fanEnabled)
	for _, s := range hwSources {
		collector.AddSource(s)
	}
	defer func() {
		if closer, ok := backend.(interface{ Close() }); ok {
			closer.Close()
		}
	}()

	// LibreHardwareMonitor HTTP（补充数据源，可关闭）。
	if *useLHM {
		collector.AddSource(newLHMSource(*lhmURL))
	}

	// ACPI / WMI（兜底，不需要驱动）
	collector.AddSource(&acpiSource{})

	// nvidia-smi（存在独显时补充 GPU 数据）
	collector.AddSource(&nvidiaSource{})

	fan := NewFanController(backend)

	// 先收拾上次异常退出留下的残局：进程被强杀时来不及把风扇交还 BIOS，
	// 芯片会停在手动模式上，过温保护也随之失效。趁还没接管任何通道，
	// 先凭磁盘上的还原点把芯片恢复成安全状态。
	if r, ok := backend.(leftoverRestorer); ok {
		if err := r.RestoreLeftover(); err != nil {
			log.Printf("[fan] 还原上次异常退出的风扇状态失败: %v", err)
		}
	}

	// 恢复上次保存的设置。用户在网页上调好的转速不该因为服务重启或
	// 机器重启就莫名其妙变回 BIOS 默认值。
	fan.EnablePersistence(fanStatePath())
	if err := fan.RestoreState(fanStatePath()); err != nil {
		log.Printf("[fan] 恢复上次保存的设置失败: %v", err)
	} else if st := fan.State(); st.Mode != FanModeAuto {
		log.Printf("[fan] 已恢复上次保存的设置：%s，通道 %d，占空比 %.0f%%",
			describeMode(st.Mode), st.Channel, st.Duty)
	}

	// 指定通道时优先于自动检测（自动检测会跟着"正在转的"那个通道走）。
	if *fanChan >= 0 {
		if err := fan.SetChannel(*fanChan); err != nil {
			log.Printf("[fan] 指定通道 %d 失败: %v", *fanChan, err)
		} else {
			log.Printf("[fan] 使用指定的风扇通道 %d", *fanChan)
		}
	}

	// 命令行参数优先于保存的设置：显式传了才覆盖。
	if *fanDuty > 0 {
		// 初始占空比做一次夹紧，避免用户传入过小的值被直接拒绝后不知情。
		initialDuty := *fanDuty
		if initialDuty < minSafeDutyPercent {
			log.Printf("[fan] 初始占空比 %v%% 低于安全下限，已提升到 %v%%",
				initialDuty, minSafeDutyPercent)
			initialDuty = minSafeDutyPercent
		}
		if initialDuty > 100 {
			initialDuty = 100
		}
		// 只有调速后端确实可用时才写寄存器，避免启动就报错。
		if info := fan.Info(); info.Available {
			if err := fan.SetDuty(initialDuty); err != nil {
				log.Printf("[fan] 初始占空比设置失败: %v", err)
			}
		}
	}
	if *fanSafety > 0 {
		if err := fan.SetSafetyTemp(*fanSafety); err != nil {
			log.Printf("[fan] 过温阈值设置被拒绝: %v", err)
		}
	}
	collector.SetFanController(fan)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 可选：自动拉起 LibreHardwareMonitor（不阻塞启动）
	if *lhmExe != "" {
		launcher := &lhmLauncher{exePath: *lhmExe, logf: log.Printf}
		go launcher.ensure(ctx, *lhmURL)
	}

	start := func() (func() error, error) {
		collectCtx, collectCancel := context.WithCancel(ctx)
		go collector.Start(collectCtx, *poll)

		srvCtx, srvCancel := context.WithCancel(ctx)
		errCh := make(chan error, 1)
		go func() { errCh <- runServer(srvCtx, *addr, collector, fan) }()

		stop := func() error {
			// 无论是控制台退出还是服务停止，都要先把风扇交还硬件，
			// 否则风扇会卡在程序设定的转速上。
			fan.Release()
			collectCancel()
			srvCancel()
			select {
			case err := <-errCh:
				return err
			case <-time.After(6 * time.Second):
				return nil
			}
		}
		return stop, nil
	}

	if *asService {
		if err := runAsService(start); err != nil {
			log.Fatalf("服务模式启动失败: %v", err)
		}
		return
	}

	stop, err := start()
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}

	log.Printf("nec8mon %s 已启动", version)
	log.Printf("  仪表盘:   http://%s", displayHost(*addr))
	log.Printf("  JSON API: http://%s/api/sensors", displayHost(*addr))

	if info := fan.Info(); info.Available {
		log.Printf("  风扇调速: 可用（%s，%s）", info.Chip, info.Detail)
	} else {
		detail := info.Detail
		if detail == "" {
			detail = "未检测到可控风扇的 Super I/O 芯片"
		}
		log.Printf("  风扇调速: 不可用 —— %s", detail)
	}

	if *openUI {
		url := "http://" + displayHost(*addr)
		if err := openBrowser(url); err != nil {
			log.Printf("[ui] 打开浏览器失败: %v", err)
		}
	}

	// 等待退出信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("收到信号 %s，正在退出…", sig)
	fan.Release()
	_ = stop()
	log.Printf("已退出")
}

// displayHost 把监听地址整理成方便点击的形式。
func displayHost(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return "127.0.0.1" + addr
	}
	return addr
}
