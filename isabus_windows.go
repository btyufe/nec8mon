//go:build windows

package main

import (
	"log"
	"runtime"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// isaBusMutexName 是全局 ISA 总线互斥量名称。
//
// LibreHardwareMonitor / OpenHardwareMonitor 使用的是同一个名字，
// 这样两个程序不会同时抢 ISA 总线 —— 并发读写 Super I/O 的运行时寄存器
// 会读到错乱的数据，甚至把风扇控制寄存器写坏。
const isaBusMutexName = `Global\Access_ISABUS.HTP.Method`

// isaBusMutex 包装 Windows 互斥量，用于串行化对 ISA 总线的访问。
type isaBusMutex struct {
	handle   windows.Handle
	timeouts atomic.Int32
}

// newISABusMutex 打开或创建 ISA 总线互斥量。
//
// 优先打开已存在的实例（LibreHardwareMonitor 等程序可能已经建好了）。
// 若不存在则自己创建一个，并授予 everyone 完全控制权限，
// 这样即使两个程序以不同权限运行，也能共用同一个互斥量。
func newISABusMutex() *isaBusMutex {
	name, err := windows.UTF16PtrFromString(isaBusMutexName)
	if err != nil {
		return nil
	}

	if h, err := windows.OpenMutex(windows.MUTEX_ALL_ACCESS, false, name); err == nil {
		return &isaBusMutex{handle: h}
	}

	// "D:(A;;GA;;;WD)" = 允许 Everyone 完全控制。
	attrs := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: nil,
		InheritHandle:      0,
	}
	if sd, err := windows.SecurityDescriptorFromString("D:(A;;GA;;;WD)"); err == nil {
		attrs.SecurityDescriptor = sd
	}

	h, err := windows.CreateMutex(attrs, false, name)
	if err != nil {
		return nil
	}
	return &isaBusMutex{handle: h}
}

// Lock 获取互斥量。返回是否成功。
//
// 【非常关键】：
// Windows Mutex 的所有权严格归属于 OS 物理线程（Thread）。
// Go 的 Goroutine 会在多个 OS 物理线程间被调度切换。如果在调用 WaitForSingleObject 时
// 发生阻塞，且当前 Goroutine 未与物理线程强绑定，Go 的 exitsyscall 可能会将该 Goroutine
// 调度到另一个 OS 物理线程继续运行。后续调 ReleaseMutex 就会报 ERROR_NOT_OWNER (288) 失败，
// 导致互斥量在原物理线程上永久死锁！
//
// 因此必须在进入 syscall 之前就调用 runtime.LockOSThread() 绑定当前物理线程，
// 这样 WaitForSingleObject 无论阻塞多久，返回后 Go 调度器绝对保证该 Goroutine
// 继续在同一个物理线程上运行，直到 Unlock 成功调用 ReleaseMutex 后再 runtime.UnlockOSThread()。
func (m *isaBusMutex) Lock(timeoutMs uint32) bool {
	if m == nil || m.handle == 0 {
		return true
	}
	// 硬件总线互斥至少给 1000ms 等待前序操作完成，杜绝虚假超时
	if timeoutMs < 1000 {
		timeoutMs = 1000
	}

	// 【核心修正】：必须在调用 WaitForSingleObject 之前就锁定 OS 物理线程！
	runtime.LockOSThread()

	res, err := windows.WaitForSingleObject(m.handle, timeoutMs)
	if err != nil || (res != windows.WAIT_OBJECT_0 && res != windows.WAIT_ABANDONED) {
		// 未能获取锁（超时或失败），必须立即解绑当前物理线程
		runtime.UnlockOSThread()
		cnt := m.timeouts.Add(1)
		if cnt%10 == 1 {
			log.Printf("[isabus] 获取 ISA 总线锁超时 (handle=0x%X, res=0x%X, err=%v, 连续超时=%d 次)",
				m.handle, res, err, cnt)
		}
		return false
	}

	m.timeouts.Store(0)
	return true
}

// Unlock 释放互斥量并解绑 OS 物理线程。
func (m *isaBusMutex) Unlock() {
	if m == nil || m.handle == 0 {
		return
	}
	defer runtime.UnlockOSThread()
	if err := windows.ReleaseMutex(m.handle); err != nil {
		log.Printf("[isabus] ReleaseMutex 失败 (thread=%d, handle=0x%X): %v",
			windows.GetCurrentThreadId(), m.handle, err)
	}
}

// Close 关闭互斥量句柄。
func (m *isaBusMutex) Close() {
	if m == nil || m.handle == 0 {
		return
	}
	_ = windows.CloseHandle(m.handle)
	m.handle = 0
}
