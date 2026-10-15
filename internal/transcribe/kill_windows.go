package transcribe

import (
	"os/exec"
	"strconv"
)

// killTreeOnCancel kills the wrapper and everything it started (PowerShell
// leaves its Python child running otherwise) on timeout or shutdown.
// ponytail: a hard kill of the proxy itself (Stop-Process) still orphans the
// tree; a Job Object with KILL_ON_JOB_CLOSE closes that if it ever bites.
func killTreeOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return cmd.Process.Kill()
	}
}
