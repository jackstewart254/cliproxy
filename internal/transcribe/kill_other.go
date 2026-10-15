//go:build !windows

package transcribe

import (
	"os/exec"
	"syscall"
)

// killTreeOnCancel puts the wrapper in its own process group and kills the
// whole group on timeout or shutdown, so no child keeps the GPU busy.
func killTreeOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
