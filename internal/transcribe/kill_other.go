//go:build !windows

package transcribe

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// killTreeOnCancel puts the wrapper in its own process group, whose id is
// its pid, and kills the whole group on timeout or shutdown, so no child
// keeps the GPU busy.
func killTreeOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// afterStart has nothing to do here: the process group already holds the tree.
func afterStart(*exec.Cmd) {}

// procStart identifies a running process by its start time, so a reused pid
// never matches; false when no process has that pid.
func procStart(pid int) (string, bool) {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	start := strings.TrimSpace(string(out))
	return start, err == nil && start != ""
}

// killTree kills a wrapper left running by a proxy that died without
// stopping it (the process group pid leads), and waits up to 10 s for it to go.
func killTree(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && syscall.Kill(pid, 0) == nil; {
		time.Sleep(50 * time.Millisecond)
	}
}
