//go:build !windows

package app

import (
	"os/exec"
	"sync"
)

// 非 Windows 沿用直接启动与结束 Node 的行为。
func startBrowserProcess(cmd *exec.Cmd) (func(), error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { _ = cmd.Process.Kill() }) }, nil
}
