//go:build windows

package main

import (
	"log"

	"golang.org/x/sys/windows/svc"
)

// nec8monService 把主程序包装成 Windows 服务。
type nec8monService struct {
	start func() (func() error, error)
}

// Execute 实现 svc.Handler。
func (s *nec8monService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptParamChange

	changes <- svc.Status{State: svc.StartPending}

	stop, err := s.start()
	if err != nil {
		changes <- svc.Status{State: svc.Stopped}
		return false, 1
	}

	changes <- svc.Status{State: svc.Running, Accepts: accepted}

	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			_ = stop()
			changes <- svc.Status{State: svc.Stopped}
			return false, 0
		default:
			log.Printf("[svc] 忽略未处理的服务控制命令 %d", c.Cmd)
		}
	}
	return false, 0
}

// runAsService 以 Windows 服务方式运行。服务名固定为 nec8mon。
func runAsService(start func() (func() error, error)) error {
	return svc.Run("nec8mon", &nec8monService{start: start})
}
