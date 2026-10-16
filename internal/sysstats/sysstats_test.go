package sysstats

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Captured from jacks-desktop's RTX 3060 (Windows, so CRLF), plus a card
// that reports no power draw.
const nvidiaSMIOutput = "NVIDIA GeForce RTX 3060, 1, 371, 12288, 45, 33.91\r\n" +
	"NVIDIA T400, 0, 5, 2048, 38, [N/A]\r\n"

func TestParseNvidiaSMI(t *testing.T) {
	got := parseNvidiaSMI([]byte(nvidiaSMIOutput))
	want := []GPU{
		{Name: "NVIDIA GeForce RTX 3060", Percent: 1, MemoryUsed: 371 << 20, MemoryTotal: 12288 << 20, Celsius: 45, Watts: 33.91},
		{Name: "NVIDIA T400", MemoryUsed: 5 << 20, MemoryTotal: 2048 << 20, Celsius: 38},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d cards, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("card %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Without nvidia-smi there are no GPUs, and the reading leaves the field out.
func TestNoNvidiaSMI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if gpus, _, _ := readSensors(context.Background()); gpus != nil {
		t.Fatalf("gpus = %+v, want none", gpus)
	}
	out, err := json.Marshal(Stats{})
	if err != nil || strings.Contains(string(out), `"gpus"`) {
		t.Fatalf("reading without GPUs = %s, %v; want no gpus field", out, err)
	}
}

// Grouping on Windows: config rules first (matching a node or python
// command line), then built in ones, then the parent's group, then
// Windows and other. On macOS an app's helpers group under its bundle.
func TestGroupProcs(t *testing.T) {
	procs := map[int32]procSample{
		4:  {name: "explorer.exe", exe: `C:\Windows\explorer.exe`},
		10: {name: "node.exe", ppid: 4, exe: `C:\Users\User\node22\node.exe`, cmdline: `node C:\Users\User\grad-jobs\scraper\node_modules\tsx\dist\cli.mjs src/daemon.ts`},
		11: {name: "cmd.exe", ppid: 10, exe: `C:\Windows\system32\cmd.exe`, cmdline: `cmd /d /s /c tsx src/daemon.ts`},
		12: {name: "python.exe", ppid: 4, exe: `C:\Users\User\whisper\python.exe`, cmdline: `python -m whisperx memo.m4a`},
		13: {name: "Razer Synapse Service.exe", exe: `C:\Program Files (x86)\Razer\Synapse3\Service\Razer Synapse Service.exe`},
		14: {name: "conhost.exe", ppid: 13, exe: `C:\Windows\system32\conhost.exe`},
		15: {name: "MsMpEng.exe"}, // a protected process: no path
		16: {name: "conhost.exe", ppid: 4, exe: `C:\Windows\system32\conhost.exe`},
		17: {name: "cliproxy-dash.exe", exe: `C:\Users\User\cliproxy\cliproxy-dash.exe`},
		18: {name: "mystery.exe", exe: `C:\Tools\mystery.exe`},
	}
	var list []Process
	for pid, p := range procs {
		list = append(list, Process{Name: p.name, PID: pid, RSS: 1 << 20, CPUPercent: 40})
	}
	rules := normalize([]Rule{
		{Name: "Grad jobs", Match: []string{"Grad-Jobs"}},
		{Name: "WhisperX", Kind: "Work", Match: []string{"whisperx"}},
		{Name: "Dropped", Match: []string{" "}},
	})
	if len(rules) != 2 || rules[0].Kind != "work" || rules[0].Match[0] != "grad-jobs" {
		t.Fatalf("rules = %+v", rules)
	}
	want := map[string]struct {
		kind  string
		count int
	}{
		"Grad jobs": {"work", 2}, "WhisperX": {"work", 1}, "Razer": {"removable", 2},
		"Windows Defender": {"system", 1}, "CLI proxy": {"work", 1}, "Windows and other": {"system", 3},
	}
	groups := groupProcs(procs, list, rules, map[int32]uint64{12: 3 << 30}, 4, "windows", 17)
	if len(groups) != len(want) {
		t.Fatalf("groups = %+v", groups)
	}
	for _, g := range groups {
		if w, ok := want[g.Name]; !ok || g.Kind != w.kind || g.Count != w.count || g.RSS != uint64(w.count)<<20 || g.CPUPercent != float64(w.count)*10 {
			t.Errorf("group %s %s: %d processes, %d bytes, %.0f%%; want %+v", g.Name, g.Kind, g.Count, g.RSS, g.CPUPercent, w)
		}
		if gpu := g.Name == "WhisperX"; g.GPU != gpu || (gpu && g.GPUMemory != 3<<30) {
			t.Errorf("group %s: gpu %v %d", g.Name, g.GPU, g.GPUMemory)
		}
	}

	mac := map[int32]procSample{
		1: {name: "Google Chrome Helper", exe: "/Applications/Google Chrome.app/Contents/Frameworks/Helpers/Google Chrome Helper.app/Contents/MacOS/Google Chrome Helper"},
		2: {name: "Finder", exe: "/System/Library/CoreServices/Finder.app/Contents/MacOS/Finder"},
		3: {name: "Google Chrome", exe: "/private/var/folders/x/com.google.Chrome.code_sign_clone/Google Chrome.app.bundle/Contents/MacOS/Google Chrome"},
		4: {name: "WebContent", exe: "/Applications/Foo/com.apple.WebKit.WebContent.xpc/Contents/MacOS/com.apple.WebKit.WebContent"},
	}
	groups = groupProcs(mac, []Process{{PID: 1, RSS: 1}, {PID: 2, RSS: 1}, {PID: 3, RSS: 1}, {PID: 4, RSS: 1}}, nil, nil, 1, "darwin", 0)
	if len(groups) != 2 || groups[0].Name != "Google Chrome" || groups[0].Count != 2 || groups[0].Kind != "app" || groups[1].Name != "System and other" || groups[1].Count != 2 {
		t.Fatalf("mac groups = %+v", groups)
	}
}
