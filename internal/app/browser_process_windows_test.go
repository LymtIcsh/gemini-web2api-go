//go:build windows

package app

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

type browserProcessTestState struct {
	Worker int `json:"worker"`
	Child  int `json:"child"`
}

// 故障目标：真实Go父退出、RPC关闭或Node崩溃后，不得遗留实际子/孙进程。
func TestBrowserProcessTreeLifetime(t *testing.T) {
	for _, mode := range []string{"owner_exit", "worker_close", "worker_exit", "worker_protocol_error"} {
		t.Run(mode, func(t *testing.T) {
			node, err := exec.LookPath("node")
			if err != nil {
				t.Fatal(err)
			}
			directory := t.TempDir()
			ready, stop := filepath.Join(directory, "ready.json"), filepath.Join(directory, "stop")
			workerPath := filepath.Join(directory, "worker.mjs")
			workerSource := `import { spawn } from 'node:child_process';
import { writeFileSync, existsSync } from 'node:fs';
import { createInterface } from 'node:readline';
spawn(process.execPath, ['-e', 'require("node:fs").writeFileSync(process.env.BROWSER_PROCESS_TEST_READY, JSON.stringify({worker:process.ppid,child:process.pid})); setInterval(() => {}, 1000000)'], { stdio: 'ignore' });
process.stdout.write(JSON.stringify({ id: '', account_id: 0, type: 'ready' }) + '\n');
const lines = createInterface({ input: process.stdin });
lines.on('line', line => { const req = JSON.parse(line); process.stdout.write(JSON.stringify({ id: req.id, account_id: req.account_id, type: 'end' }) + '\n'); });
let signalled = false;
setInterval(() => {
  if (signalled || !existsSync(process.env.BROWSER_PROCESS_TEST_STOP)) return;
  signalled = true;
  if (process.env.BROWSER_PROCESS_TEST_MODE === 'worker_exit') process.exit(12);
  if (process.env.BROWSER_PROCESS_TEST_MODE === 'worker_protocol_error') process.stdout.write('invalid-json\n');
}, 20);
`
			if err := os.WriteFile(workerPath, []byte(workerSource), 0600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			owner := exec.Command(executable, "-test.run=^TestBrowserProcessOwnerHelper$")
			owner.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
			owner.Env = append(os.Environ(), "BROWSER_PROCESS_TEST_OWNER=1", "BROWSER_NODE="+node,
				"BROWSER_WORKER_PATH="+workerPath, "BROWSER_PROCESS_TEST_READY="+ready,
				"BROWSER_PROCESS_TEST_STOP="+stop, "BROWSER_PROCESS_TEST_MODE="+mode)
			owner.Stdout, owner.Stderr = os.Stdout, os.Stderr
			if err := owner.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
			var state browserProcessTestState
			deadline := time.Now().Add(10 * time.Second)
			for {
				data, readErr := os.ReadFile(ready)
				if readErr == nil && json.Unmarshal(data, &state) == nil && state.Worker > 0 && state.Child > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("真实Node子树未就绪")
				}
				time.Sleep(10 * time.Millisecond)
			}
			handles := make([]windows.Handle, 0, 2)
			for _, pid := range []int{state.Worker, state.Child} {
				handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
				if err != nil {
					t.Fatal(err)
				}
				handles = append(handles, handle)
				t.Cleanup(func() {
					if status, _ := windows.WaitForSingleObject(handle, 0); status == uint32(windows.WAIT_TIMEOUT) {
						_ = exec.Command("taskkill.exe", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
					}
					_ = windows.CloseHandle(handle)
				})
			}
			if mode == "owner_exit" {
				if err := owner.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(stop, nil, 0600); err != nil {
				t.Fatal(err)
			}
			for i, handle := range handles {
				status, err := windows.WaitForSingleObject(handle, 5000)
				if err != nil || status != windows.WAIT_OBJECT_0 {
					t.Fatalf("模式=%s 第%d个进程仍驻留，状态=%d 错误=%v", mode, i+1, status, err)
				}
			}
		})
	}
}

// 独立测试二进制作为工作进程所有者；只有父测试的显式环境才运行。
func TestBrowserProcessOwnerHelper(t *testing.T) {
	if os.Getenv("BROWSER_PROCESS_TEST_OWNER") != "1" {
		t.Skip("仅供真实子树生命周期测试调用")
	}
	if _, err := managedBrowserRPC(); err != nil {
		t.Fatal(err)
	}
	closed := false
	for {
		if !closed && os.Getenv("BROWSER_PROCESS_TEST_MODE") == "worker_close" {
			if _, err := os.Stat(os.Getenv("BROWSER_PROCESS_TEST_STOP")); err == nil {
				closeBrowserWorker()
				fmt.Fprintln(os.Stdout, "工作进程关闭完成")
				closed = true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}
