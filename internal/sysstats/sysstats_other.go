//go:build !windows

package sysstats

import (
	"context"
	"strings"

	"github.com/shirou/gopsutil/v4/disk"
)

// threadCounts is nil here: gopsutil reads each process's threads cheaply.
func threadCounts() map[int32]int32 { return nil }

// mounts lists the volumes worth showing. On macOS the system's own volumes
// and the simulator images are left out.
// ponytail: a dead NFS mount can stall this on Linux; filter by Fstype if it bites.
func mounts(ctx context.Context) []string {
	parts, _ := disk.PartitionsWithContext(ctx, false)
	var out []string
	for _, p := range parts {
		m := p.Mountpoint
		if m == "" || p.Fstype == "devfs" || p.Fstype == "autofs" || strings.HasPrefix(m, "/System/Volumes/") || strings.Contains(m, "/CoreSimulator/") {
			continue
		}
		out = append(out, m)
	}
	return out
}
