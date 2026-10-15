//go:build !windows

package sysstats

// threadCounts is nil here: gopsutil reads each process's threads cheaply.
func threadCounts() map[int32]int32 { return nil }
