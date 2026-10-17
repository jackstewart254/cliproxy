//go:build windows

package sysstats

import (
	"context"
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

// mounts lists the fixed drives ("C:\"). Network, removable and optical
// drives are left out without being touched: a dead mapped drive can block
// a volume or free-space query far past the caller's timeout.
func mounts(context.Context) []string {
	buf := make([]uint16, 256) // 26 drives of "X:\" and a NUL each
	n, err := windows.GetLogicalDriveStrings(uint32(len(buf)), &buf[0])
	if err != nil || int(n) > len(buf) {
		return nil
	}
	var out []string
	for start := 0; start < int(n); {
		end := start
		for end < int(n) && buf[end] != 0 {
			end++
		}
		if end > start && windows.GetDriveType(&buf[start]) == windows.DRIVE_FIXED {
			out = append(out, windows.UTF16ToString(buf[start:end]))
		}
		start = end + 1
	}
	return out
}
