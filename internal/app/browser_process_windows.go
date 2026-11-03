//go:build windows

package app

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 暂停 Node 后先纳入非继承 Job，避免它抢先创建未归属的浏览器子孙。
func startBrowserProcess(cmd *exec.Cmd) (func(), error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("创建浏览器进程 Job: %w", err)
	}
	var once sync.Once
	stop := func() { once.Do(func() { _ = windows.CloseHandle(job) }) }
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		stop()
		return nil, fmt.Errorf("设置浏览器进程 Job: %w", err)
	}
	attributes := syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		attributes = *cmd.SysProcAttr
	}
	attributes.HideWindow = true
	attributes.CreationFlags |= windows.CREATE_SUSPENDED
	cmd.SysProcAttr = &attributes
	if err := cmd.Start(); err != nil {
		stop()
		return nil, err
	}
	// 绑定或恢复失败时，暂停进程也必须终止并 Wait，回收 exec 持有的管道和句柄。
	started := false
	defer func() {
		if !started {
			_ = cmd.Process.Kill()
			stop()
			_ = cmd.Wait()
		}
	}()
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return nil, fmt.Errorf("打开浏览器进程: %w", err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		return nil, fmt.Errorf("绑定浏览器进程 Job: %w", err)
	}
	if err := resumeBrowserProcess(uint32(cmd.Process.Pid)); err != nil {
		return nil, err
	}
	started = true
	return stop, nil
}

// Go 的 cmd.Start 已关闭初始线程句柄；暂停进程尚未运行，通过快照重新打开该线程。
func resumeBrowserProcess(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("读取浏览器进程线程: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return fmt.Errorf("打开浏览器初始线程: %w", err)
		}
		defer windows.CloseHandle(thread)
		if _, err := windows.ResumeThread(thread); err != nil {
			return fmt.Errorf("恢复浏览器初始线程: %w", err)
		}
		return nil
	}
	return fmt.Errorf("查找浏览器初始线程: %w", err)
}
