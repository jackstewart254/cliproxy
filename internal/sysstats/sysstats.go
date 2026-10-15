// Package sysstats reads the load on the host this proxy runs on: CPU,
// memory, disks and the busiest processes, for the dashboard's Devices tab.
package sysstats

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
)

// Stats is one reading of the host.
type Stats struct {
	Hostname        string        `json:"hostname"`
	OS              string        `json:"os"`
	Platform        string        `json:"platform"`
	PlatformVersion string        `json:"platform_version"`
	Arch            string        `json:"arch"`
	UptimeSeconds   uint64        `json:"uptime_seconds"`
	CPU             CPU           `json:"cpu"`
	Load            *load.AvgStat `json:"load"` // nil on Windows, which has no load average
	Memory          Memory        `json:"memory"`
	Swap            Memory        `json:"swap"`
	Disks           []Disk        `json:"disks"`
	Processes       int           `json:"processes"`
	Threads         int           `json:"threads"`
	// Unreadable counts processes this user may not inspect (on macOS,
	// other users' processes); they are missing from threads and the tops.
	Unreadable    int       `json:"unreadable"`
	TopCPU        []Process `json:"top_cpu"`
	TopMemory     []Process `json:"top_memory"`
	Headroom      Headroom  `json:"headroom"`
	SampledAt     time.Time `json:"sampled_at"`
	SampleSeconds float64   `json:"sample_seconds"` // the span CPU percentages cover
}

// CPU is the processor's busy share over the sample, in total and per core.
type CPU struct {
	Model   string    `json:"model"`
	Cores   int       `json:"cores"`
	Percent float64   `json:"percent"`
	PerCore []float64 `json:"per_core"`
}

// Memory is RAM or swap, in bytes.
type Memory struct {
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Available uint64  `json:"available,omitempty"`
	Percent   float64 `json:"percent"`
}

// Disk is one mounted volume, in bytes.
type Disk struct {
	Mount string `json:"mount"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

// Process is one process. CPUPercent is a share of one core, as top shows
// it, so a busy multi-threaded process passes 100.
type Process struct {
	Name       string  `json:"name"`
	PID        int32   `json:"pid"`
	CPUPercent float64 `json:"cpu_percent"`
	RSS        uint64  `json:"rss"`
	Threads    int32   `json:"threads"`
}

// Headroom is what is left for more work.
type Headroom struct {
	CPUIdlePercent         float64 `json:"cpu_idle_percent"`
	IdleCores              float64 `json:"idle_cores"`
	MemoryAvailable        uint64  `json:"memory_available"`
	MemoryAvailablePercent float64 `json:"memory_available_percent"`
}

const (
	topN = 15
	// Readings closer together than minGap share one; the dashboard asks
	// every few seconds and two callers should not sample twice.
	minGap = time.Second
	// CPU percentages compare two samples. Each reading uses the last one
	// as its start; when that is older than staleAfter, a fresh one is
	// taken primeGap before the reading.
	primeGap   = 250 * time.Millisecond
	staleAfter = 10 * time.Second
	diskEvery  = time.Minute
)

type sample struct {
	at    time.Time
	cores []cpu.TimesStat
	procs map[int32]procSample
}

type procSample struct {
	name    string
	cpu     float64 // seconds used since the process started
	rss     uint64
	threads int32
}

var reader struct {
	sync.Mutex
	static  *Stats // fields that never change
	prev    *sample
	last    *Stats
	disks   []Disk
	disksAt time.Time
}

// Read returns a reading of this host. It never runs a goroutine of its own:
// CPU percentages cover the time since the previous call, or primeGap when
// that is long ago.
func Read(ctx context.Context) (*Stats, error) {
	reader.Lock()
	defer reader.Unlock()
	if reader.last != nil && time.Since(reader.last.SampledAt) < minGap {
		return reader.last, nil
	}
	cur, err := takeSample(ctx)
	if err != nil {
		return nil, err
	}
	prev := reader.prev
	if prev == nil || cur.at.Sub(prev.at) > staleAfter {
		prev = cur
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(primeGap):
		}
		if cur, err = takeSample(ctx); err != nil {
			return nil, err
		}
	}
	reader.prev = cur
	reader.last = build(ctx, prev, cur)
	return reader.last, nil
}

func takeSample(ctx context.Context) (*sample, error) {
	cores, err := cpu.TimesWithContext(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("read cpu times: %w", err)
	}
	// A failed process list leaves the tops empty; the totals still count.
	procs, _ := process.ProcessesWithContext(ctx)
	threads := threadCounts()
	out := &sample{at: time.Now(), cores: cores, procs: make(map[int32]procSample, len(procs))}
	for _, p := range procs {
		var ps procSample
		ps.name, _ = p.NameWithContext(ctx)
		if t, errTimes := p.TimesWithContext(ctx); errTimes == nil {
			ps.cpu = t.User + t.System
		}
		if m, errMem := p.MemoryInfoWithContext(ctx); errMem == nil {
			ps.rss = m.RSS
		}
		if threads != nil {
			ps.threads = threads[p.Pid]
		} else {
			ps.threads, _ = p.NumThreadsWithContext(ctx)
		}
		out.procs[p.Pid] = ps
	}
	return out, nil
}

// busy splits the time between two CPU samples into busy and total seconds.
func busy(a, b cpu.TimesStat) (float64, float64) {
	work := func(t cpu.TimesStat) float64 { return t.User + t.System + t.Nice + t.Irq + t.Softirq + t.Steal }
	idle := func(t cpu.TimesStat) float64 { return t.Idle + t.Iowait }
	w := work(b) - work(a)
	total := w + idle(b) - idle(a)
	if w < 0 || total <= 0 {
		return 0, 0
	}
	return min(w, total), total
}

func percent(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	return part / whole * 100
}

func build(ctx context.Context, prev, cur *sample) *Stats {
	if reader.static == nil {
		reader.static = hostInfo(ctx)
	}
	s := *reader.static
	s.SampledAt = cur.at
	s.SampleSeconds = cur.at.Sub(prev.at).Seconds()

	var allBusy, allTotal float64
	for i := 0; i < len(cur.cores) && i < len(prev.cores); i++ {
		b, t := busy(prev.cores[i], cur.cores[i])
		allBusy += b
		allTotal += t
		s.CPU.PerCore = append(s.CPU.PerCore, percent(b, t))
	}
	s.CPU.Cores = len(s.CPU.PerCore)
	s.CPU.Percent = percent(allBusy, allTotal)

	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		s.Memory = Memory{Total: vm.Total, Used: vm.Used, Available: vm.Available, Percent: vm.UsedPercent}
	}
	if sw, err := mem.SwapMemoryWithContext(ctx); err == nil {
		s.Swap = Memory{Total: sw.Total, Used: sw.Used, Percent: sw.UsedPercent}
	}
	if runtime.GOOS != "windows" {
		// gopsutil emulates it on Windows with a goroutine that never stops.
		s.Load, _ = load.AvgWithContext(ctx)
	}
	s.UptimeSeconds, _ = host.UptimeWithContext(ctx)
	if time.Since(reader.disksAt) > diskEvery {
		reader.disks, reader.disksAt = readDisks(ctx), time.Now()
	}
	s.Disks = reader.disks

	list := make([]Process, 0, len(cur.procs))
	for pid, p := range cur.procs {
		s.Threads += int(p.threads)
		if p.rss == 0 {
			s.Unreadable++
			continue
		}
		proc := Process{Name: p.name, PID: pid, RSS: p.rss, Threads: p.threads}
		if was, ok := prev.procs[pid]; ok && was.name == p.name && s.SampleSeconds > 0 {
			proc.CPUPercent = max(0, (p.cpu-was.cpu)/s.SampleSeconds*100)
		}
		if proc.Name == "" {
			proc.Name = fmt.Sprintf("pid %d", pid)
		}
		list = append(list, proc)
	}
	s.Processes = len(cur.procs)
	s.TopCPU = top(list, func(a, b Process) bool { return a.CPUPercent > b.CPUPercent })
	s.TopMemory = top(list, func(a, b Process) bool { return a.RSS > b.RSS })

	idle := 100 - s.CPU.Percent
	s.Headroom = Headroom{
		CPUIdlePercent:         idle,
		IdleCores:              idle / 100 * float64(s.CPU.Cores),
		MemoryAvailable:        s.Memory.Available,
		MemoryAvailablePercent: percent(float64(s.Memory.Available), float64(s.Memory.Total)),
	}
	return &s
}

// top returns the first topN of list in the order less gives, ties by pid.
func top(list []Process, less func(a, b Process) bool) []Process {
	out := append([]Process(nil), list...)
	sort.Slice(out, func(i, j int) bool {
		if less(out[i], out[j]) != less(out[j], out[i]) {
			return less(out[i], out[j])
		}
		return out[i].PID < out[j].PID
	})
	return out[:min(topN, len(out))]
}

func hostInfo(ctx context.Context) *Stats {
	s := &Stats{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if name, err := os.Hostname(); err == nil {
		s.Hostname, _, _ = strings.Cut(name, ".")
	}
	if info, err := host.InfoWithContext(ctx); err == nil {
		s.Platform, s.PlatformVersion = info.Platform, info.PlatformVersion
	}
	if infos, err := cpu.InfoWithContext(ctx); err == nil && len(infos) > 0 {
		s.CPU.Model = strings.TrimSpace(infos[0].ModelName)
	}
	return s
}

// readDisks lists the volumes worth showing. On macOS the system's own
// volumes and the simulator images are left out, and APFS volumes sharing a
// container (same size and free space) show once.
// ponytail: a dead network drive on Windows can stall this; it runs once a
// minute under a deadline, skip DRIVE_REMOTE if that ever bites.
func readDisks(ctx context.Context) []Disk {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// Windows returns per-drive warnings as an error next to the list.
	parts, _ := disk.PartitionsWithContext(ctx, false)
	seen := make(map[[2]uint64]bool)
	var out []Disk
	for _, p := range parts {
		m := p.Mountpoint
		if m == "" || p.Fstype == "devfs" || p.Fstype == "autofs" || strings.HasPrefix(m, "/System/Volumes/") || strings.Contains(m, "/CoreSimulator/") {
			continue
		}
		u, err := disk.UsageWithContext(ctx, m)
		if err != nil || u.Total == 0 || seen[[2]uint64{u.Total, u.Free}] {
			continue
		}
		seen[[2]uint64{u.Total, u.Free}] = true
		out = append(out, Disk{Mount: m, Total: u.Total, Free: u.Free})
	}
	return out
}
