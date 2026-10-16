package transcribe

import (
	"os/exec"
	"strconv"
	"sync"
	"unsafe"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
)

// killTreeOnCancel kills the wrapper and everything it started (PowerShell
// leaves its Python child running otherwise) on timeout or shutdown.
func killTreeOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return cmd.Process.Kill()
	}
}

// wrapperJob holds every wrapper this proxy starts. Its handle stays open
// for the proxy's life; when the proxy dies, by any means, Windows closes it
// and kills everything in the job, so no WhisperX outlives a Stop-Process.
var wrapperJob = sync.OnceValues(func() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
})

// afterStart puts the started wrapper in wrapperJob; its children inherit
// the job. A child spawned between start and this call would escape, but
// PowerShell takes far longer than that to start Python, and recover's
// pid check backs this up.
func afterStart(cmd *exec.Cmd) {
	job, err := wrapperJob()
	if err != nil {
		log.Warnf("transcription: no kill-on-exit job for the wrapper: %v", err)
		return
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, proc)
		_ = windows.CloseHandle(proc)
	}
	if err != nil {
		log.Warnf("transcription: wrapper not in the kill-on-exit job: %v", err)
	}
}

// procStart identifies a running process by its creation time, so a reused
// pid never matches; false when no process with that pid is running.
func procStart(pid int) (string, bool) {
	proc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return "", false
	}
	defer func() { _ = windows.CloseHandle(proc) }()
	if event, errWait := windows.WaitForSingleObject(proc, 0); errWait != nil || event != uint32(windows.WAIT_TIMEOUT) {
		return "", false
	}
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(proc, &created, &exited, &kernel, &user) != nil {
		return "", false
	}
	return strconv.FormatInt(created.Nanoseconds(), 10), true
}

// killTree kills a wrapper tree left running by a proxy that died without
// stopping it, and waits up to 10 s for it to go.
func killTree(pid int) {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
	if proc, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)); err == nil {
		_, _ = windows.WaitForSingleObject(proc, 10000)
		_ = windows.CloseHandle(proc)
	}
}
