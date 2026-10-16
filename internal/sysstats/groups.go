package sysstats

import (
	"sort"
	"strings"
)

// Group is the processes that belong to one thing: a job of yours, an app,
// a vendor's background tools, a driver or the system itself.
type Group struct {
	Name       string  `json:"name"`
	Kind       string  `json:"kind"` // work, app, remote, removable, driver or system
	About      string  `json:"about"`
	CPUPercent float64 `json:"cpu_percent"` // share of the whole processor
	RSS        uint64  `json:"rss"`
	// GPU is true when one of its processes computes on an NVIDIA card;
	// GPUMemory is what they hold there, when the driver says (Windows
	// usually does not).
	GPU       bool      `json:"gpu,omitempty"`
	GPUMemory uint64    `json:"gpu_memory,omitempty"`
	Count     int       `json:"count"`
	Processes []Process `json:"processes"` // the largest by memory, at most groupTop
}

// Rule puts the processes it matches in a group. Match holds lower case
// substrings of a process's name, program path or command line; Names holds
// exact lower case names without ".exe".
type Rule struct {
	Name, Kind, About string
	Match, Names      []string
}

// Kinds a rule may have; anything else counts as work.
var kinds = map[string]bool{"work": true, "app": true, "remote": true, "removable": true, "driver": true, "system": true}

const groupTop = 20

// builtin covers what commonly runs on the owner's machines. Rules from the
// config come first, so they can claim anything here, this proxy included.
var builtin = []Rule{
	{Name: "Claude Code", Kind: "work", About: "Claude's coding sessions and the tools they start.",
		Names: []string{"claude"}, Match: []string{`\.claude\remote`, "/.claude/remote", "claude-code"}},
	{Name: "Tailscale and SSH", Kind: "remote", About: "Lets your other devices reach this one privately.",
		Names: []string{"sshd", "sshd-session", "sftp-server"}, Match: []string{"tailscale"}},
	{Name: "Razer", Kind: "removable", About: "Razer mouse and keyboard software: lighting, macros and game tools.",
		Match: []string{`\razer`}},
	{Name: "Corsair iCUE", Kind: "removable", About: "Corsair lighting and fan software.",
		Match: []string{`\corsair`, "icue"}},
	{Name: "ASUS Armoury Crate", Kind: "removable", About: "ASUS motherboard lighting, fan and update tools.",
		Match: []string{`\asus\`, "armoury", `\lightingservice\`}},
	{Name: "BlueStacks", Kind: "removable", About: "Android app player that keeps running in the background.",
		Match: []string{"bluestacks"}},
	{Name: "Games and launchers", Kind: "removable", About: "Game stores and Xbox services waiting in the background.",
		Match: []string{`\steam\`, `\steamlibrary\`, "epicgames", `\epic games\`, "battle.net", `\riot games\`, `\riot vanguard\`, `\origin\`, `\electronic arts\`, "ubisoft", "microsoft.gamingapp", "microsoft.gamingservices", "microsoft.xbox"}},
	{Name: "Other background apps", Kind: "removable", About: "Phone Link, media apps and other extras that start with Windows.",
		Match: []string{"microsoft.yourphone", "phoneexperiencehost", "crossdevice", "microsoft.zune", "spotify", "discord", "msteams", "clipchamp", "microsoft.copilot", "cloudflared"}},
	{Name: "Ollama", Kind: "app", About: "Runs AI models on this machine.",
		Match: []string{"ollama"}},
	{Name: "Claude app", Kind: "app", About: "The Claude desktop app.",
		Match: []string{`\windowsapps\claude_`}},
	{Name: "NVIDIA driver and app", Kind: "driver", About: "Runs the graphics card, plus the NVIDIA App overlay and recording.",
		Match: []string{"nvidia", `\nvcontainer`, "nvdisplay.container"}},
	{Name: "Logitech", Kind: "driver", About: "Logitech mouse and keyboard software.",
		Match: []string{"logitech", "logioptions"}},
	{Name: "Windows Defender", Kind: "system", About: "Windows' built in antivirus. Keep it.",
		Names: []string{"msmpeng", "nissrv", "mpdefendercoreservice", "securityhealthservice", "securityhealthsystray", "defendersessionhelper", "smartscreen"},
		Match: []string{`\windows defender\`}},
	{Name: "Edge WebView2", Kind: "system", About: "The web engine other apps use to draw their windows.",
		Names: []string{"msedgewebview2"}},
}

// self is this proxy's own process.
var self = Rule{Name: "CLI proxy", Kind: "work", About: "This proxy and its dashboard."}

// fallback is the group for whatever no rule claims.
func fallback(goos string) Rule {
	if goos == "windows" {
		return Rule{Name: "Windows and other", Kind: "system", About: "Windows itself, its services, and anything not in a group above."}
	}
	return Rule{Name: "System and other", Kind: "system", About: "The system itself and anything not in a group above."}
}

// scripted reports whether a program runs code it was given, so that its
// path says little and its command line says what it is running.
func scripted(name string) bool {
	switch name {
	case "node", "py", "powershell", "pwsh", "cmd", "bash", "sh", "zsh", "java", "javaw", "ruby", "perl", "deno", "bun", "uv", "tsx":
		return true
	}
	return strings.HasPrefix(name, "python")
}

// baseName is a process name in lower case without ".exe".
func baseName(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), ".exe")
}

func (r *Rule) matches(name, hay string) bool {
	for _, n := range r.Names {
		if n == name {
			return true
		}
	}
	for _, m := range r.Match {
		if m != "" && strings.Contains(hay, m) {
			return true
		}
	}
	return false
}

// groupProcs sorts the readable processes into groups. A process no rule
// claims joins its parent's group, unless that is the system's, so the
// shells, consoles and helpers a job starts count toward the job. On macOS
// an app's helpers group under the app's bundle. The rest is the fallback.
func groupProcs(procs map[int32]procSample, list []Process, rules []Rule, gpuPIDs map[int32]uint64, cores int, goos string, selfPID int32) []Group {
	rest := fallback(goos)
	of := make(map[int32]*Rule, len(procs))
	var classify func(pid int32, depth int) *Rule
	classify = func(pid int32, depth int) *Rule {
		if r, ok := of[pid]; ok {
			return r
		}
		p, ok := procs[pid]
		if !ok || depth > 32 {
			return &rest
		}
		of[pid] = &rest // a parent loop ends here
		name := baseName(p.name)
		hay := strings.ToLower(p.name + "\n" + p.exe + "\n" + p.cmdline)
		var r *Rule
		for i := 0; r == nil && i < len(rules); i++ {
			if rules[i].matches(name, hay) {
				r = &rules[i]
			}
		}
		if r == nil && selfPID > 0 && pid == selfPID {
			r = &self
		}
		for i := 0; r == nil && i < len(builtin); i++ {
			if builtin[i].matches(name, hay) {
				r = &builtin[i]
			}
		}
		if r == nil && p.ppid > 0 && p.ppid != pid {
			if up := classify(p.ppid, depth+1); up.Kind != "system" {
				r = up
			}
		}
		if r == nil {
			r = bundle(p.exe)
		}
		if r == nil {
			r = &rest
		}
		of[pid] = r
		return r
	}

	byName := make(map[string]*Group)
	for _, proc := range list {
		r := classify(proc.PID, 0)
		g := byName[r.Name]
		if g == nil {
			g = &Group{Name: r.Name, Kind: r.Kind, About: r.About}
			byName[r.Name] = g
		}
		g.Count++
		g.RSS += proc.RSS
		g.CPUPercent += proc.CPUPercent
		g.Processes = append(g.Processes, proc)
		if mem, ok := gpuPIDs[proc.PID]; ok {
			g.GPU = true
			g.GPUMemory += mem
		}
	}
	out := make([]Group, 0, len(byName))
	for _, g := range byName {
		if cores > 0 {
			g.CPUPercent = min(100, g.CPUPercent/float64(cores))
		}
		sort.Slice(g.Processes, func(i, j int) bool { return g.Processes[i].RSS > g.Processes[j].RSS })
		g.Processes = g.Processes[:min(groupTop, len(g.Processes))]
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RSS != out[j].RSS {
			return out[i].RSS > out[j].RSS
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// bundle groups a macOS app's processes under its outermost bundle, the
// folder before the first "/Contents/": "Google Chrome.app", or the
// "Google Chrome.app.bundle" Chrome runs from while it updates. Apple's own
// (Finder, Dock, XProtect, Safari's web content) stay with the system.
func bundle(exe string) *Rule {
	if strings.Contains(exe, "/System/Library/") || strings.HasPrefix(exe, "/Library/Apple/") {
		return nil
	}
	dir, _, ok := strings.Cut(exe, "/Contents/")
	folder := strings.TrimSuffix(dir[strings.LastIndex(dir, "/")+1:], ".bundle")
	name, app := strings.CutSuffix(folder, ".app")
	if !ok || !app || name == "" {
		return nil
	}
	return &Rule{Name: strings.ReplaceAll(name, "_", " "), Kind: "app"}
}

// normalize readies rules from the config: names trimmed, kinds checked,
// matches in lower case. An entry without a name or a match is dropped.
func normalize(entries []Rule) []Rule {
	var out []Rule
	for _, e := range entries {
		r := Rule{Name: strings.TrimSpace(e.Name), Kind: strings.ToLower(strings.TrimSpace(e.Kind)), About: strings.TrimSpace(e.About)}
		for _, m := range e.Match {
			if m = strings.ToLower(strings.TrimSpace(m)); m != "" {
				r.Match = append(r.Match, m)
			}
		}
		if r.Name == "" || len(r.Match) == 0 {
			continue
		}
		if !kinds[r.Kind] {
			r.Kind = "work"
		}
		if r.About == "" && r.Kind == "work" {
			r.About = "One of your own jobs."
		}
		out = append(out, r)
	}
	return out
}
