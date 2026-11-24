//go:build windows

package runner

import (
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func newShellCmd(cwd, command string) *exec.Cmd {
	cmd := exec.Command("cmd.exe", "/d", "/s", "/c", command)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

func killProcessTree(cmd *exec.Cmd, done <-chan error) error {
	if cmd.Process == nil {
		return <-done
	}
	killer := exec.Command("taskkill.exe", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
	killer.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = killer.Run()
	select {
	case err := <-done:
		return err
	case <-time.After(800 * time.Millisecond):
		_ = cmd.Process.Kill()
		return <-done
	}
}
