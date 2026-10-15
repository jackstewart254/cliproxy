//go:build windows

package sysstats

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// threadCounts reads every process's thread count from one snapshot.
// gopsutil takes a snapshot of all processes for each one it asks about.
func threadCounts() map[int32]int32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	out := make(map[int32]int32)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		out[int32(pe.ProcessID)] = int32(pe.Threads)
	}
	return out
}
