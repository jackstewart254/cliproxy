package usagestats

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// testClock is a controllable clock for stores under test.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

func newClockStore(clock *testClock) *Store {
	store := newStore()
	store.nowFunc = clock.Now
	store.machineName = func(string) string { return "" }
	return store
}

var testZone = time.FixedZone("BST", 3600)

func TestAccountDailyHasFourteenLocalDays(t *testing.T) {
	now := time.Date(2026, 10, 4, 15, 30, 0, 0, testZone)
	store := newTestStore(now)
	store.machineName = func(string) string { return "" }
	// 00:30 local today is 23:30 UTC yesterday: it must count as today.
	store.record(coreusage.Record{AuthID: "a", RequestedAt: time.Date(2026, 10, 4, 0, 30, 0, 0, testZone), Detail: coreusage.Detail{OutputTokens: 5}})
	store.record(coreusage.Record{AuthID: "a", RequestedAt: now.Add(-time.Hour), Failed: true})
	store.record(coreusage.Record{AuthID: "a", RequestedAt: time.Date(2026, 10, 3, 12, 0, 0, 0, testZone)})
	store.record(coreusage.Record{AuthID: "a", RequestedAt: time.Date(2026, 9, 21, 9, 0, 0, 0, testZone), Detail: coreusage.Detail{InputTokens: 40}})

	daily := store.Summary(10).Accounts["a"].Daily
	if len(daily) != 14 {
		t.Fatalf("daily has %d days, want 14", len(daily))
	}
	if daily[0].Date != "2026-09-21" || daily[13].Date != "2026-10-04" {
		t.Fatalf("daily runs %s to %s, want 2026-09-21 to 2026-10-04", daily[0].Date, daily[13].Date)
	}
	if got := daily[13]; got.Requests != 2 || got.Failed != 1 || got.Output != 5 {
		t.Fatalf("today = %+v, want 2 requests, 1 failed, 5 output tokens", got)
	}
	if daily[12].Requests != 1 || daily[0].Input != 40 {
		t.Fatalf("yesterday = %+v, first day = %+v", daily[12], daily[0])
	}
	for i := 1; i < 12; i++ {
		if daily[i].Requests != 0 {
			t.Fatalf("day %s = %+v, want empty", daily[i].Date, daily[i])
		}
	}
}

func sessionsByID(summary Summary) map[string]Session {
	sessions := make(map[string]Session, len(summary.Sessions))
	for _, session := range summary.Sessions {
		sessions[session.ID] = session
	}
	return sessions
}

func TestSessionParentModelAndAuthBreakdown(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)}
	store := newClockStore(clock)
	store.record(coreusage.Record{AuthID: "claude-a", SessionID: "claude:s1", Model: "claude-sonnet-5-5", RequestedAt: clock.now.Add(-3 * time.Minute), TTFT: 800 * time.Millisecond})
	store.record(coreusage.Record{AuthID: "claude-a", SessionID: "claude:s1", Model: "claude-opus-5-5", RequestedAt: clock.now.Add(-2 * time.Minute), Failed: true})
	store.record(coreusage.Record{AuthID: "claude-b", SessionID: "claude:s1", Model: "claude-opus-5-5", RequestedAt: clock.now.Add(-time.Minute), TTFT: 1200 * time.Millisecond})
	store.record(coreusage.Record{AuthID: "claude-b", SessionID: "claude:s1:agent:x9", RequestedAt: clock.now})

	sessions := sessionsByID(store.Summary(10))
	main := sessions["claude:s1"]
	if main.ParentID != "" {
		t.Fatalf("main parent = %q, want empty", main.ParentID)
	}
	if main.Model != "claude-opus-5-5" || main.ServingAuthID != "claude-b" {
		t.Fatalf("model = %q serving = %q, want the latest model and the last account that answered", main.Model, main.ServingAuthID)
	}
	if got := main.ByAuth["claude-a"]; got == nil || got.Requests != 2 || got.Failed != 1 {
		t.Fatalf("by_auth[claude-a] = %+v, want 2 requests and 1 failure", got)
	}
	if got := main.ByAuth["claude-b"]; got == nil || got.Requests != 1 || got.Failed != 0 {
		t.Fatalf("by_auth[claude-b] = %+v, want 1 request", got)
	}
	// Two TTFTs (800, 1200): the median is the lower one, within bucket error.
	if math.Abs(float64(main.TTFTP50)-800) > 800*0.06 {
		t.Fatalf("ttft p50 = %d, want about 800", main.TTFTP50)
	}
	if main.TTFTHist != nil || main.ClientIP != "" {
		t.Fatal("summary leaks internal session fields")
	}
	agent := sessions["claude:s1:agent:x9"]
	if agent.ParentID != "claude:s1" {
		t.Fatalf("agent parent = %q, want claude:s1", agent.ParentID)
	}
	if agent.TTFTP50 != 0 || agent.ByAuth == nil {
		t.Fatalf("agent = %+v, want 0 ttft when unknown and a non-nil by_auth", agent)
	}
}

func TestLocalMachineName(t *testing.T) {
	if got := localMachineName("127.0.0.1"); got != HostName() {
		t.Fatalf("loopback = %q, want %q", got, HostName())
	}
	if got := localMachineName("192.0.2.1"); got != "" {
		t.Fatalf("other address = %q, want empty", got)
	}
}

func TestSessionMetadataMergesEvenWhenPushedFirst(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)}
	store := newClockStore(clock)
	store.machineName = func(ip string) string {
		if ip == "127.0.0.1" {
			return "this-mac"
		}
		return ""
	}
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	store.path = path

	// Pushed before the proxy has seen the session.
	if got := store.UpdateSessionMeta("mbp-m3", []SessionMetaUpdate{
		{ID: "f9703c88", Title: "CLI proxy UI redesign"},
		{ID: "codex:c1", Title: "Codex task"},
		{ID: "  ", Title: "skipped"},
	}); got != 2 {
		t.Fatalf("updated = %d, want 2", got)
	}
	store.recordFrom(coreusage.Record{AuthID: "claude-a", SessionID: "claude:f9703c88", RequestedAt: clock.now}, "127.0.0.1")
	store.recordFrom(coreusage.Record{AuthID: "claude-a", SessionID: "claude:f9703c88:agent:a1", RequestedAt: clock.now}, "127.0.0.1")
	store.recordFrom(coreusage.Record{AuthID: "codex-a", SessionID: "codex:c1", RequestedAt: clock.now}, "")
	store.recordFrom(coreusage.Record{AuthID: "codex-a", SessionID: "codex:other", RequestedAt: clock.now}, "127.0.0.1")

	check := func(store *Store) {
		t.Helper()
		sessions := sessionsByID(store.Summary(10))
		if got := sessions["claude:f9703c88"]; got.Title != "CLI proxy UI redesign" || got.Machine != "mbp-m3" {
			t.Fatalf("main session title %q machine %q, want the pushed values", got.Title, got.Machine)
		}
		if got := sessions["claude:f9703c88:agent:a1"]; got.Title != "" || got.Machine != "mbp-m3" {
			t.Fatalf("agent thread title %q machine %q, want no title and the inherited machine", got.Title, got.Machine)
		}
		if got := sessions["codex:c1"]; got.Title != "Codex task" || got.Machine != "mbp-m3" {
			t.Fatalf("codex session title %q machine %q", got.Title, got.Machine)
		}
		if got := sessions["codex:other"]; got.Machine != "this-mac" || got.Title != "" {
			t.Fatalf("unpushed session machine %q title %q, want this host's name", got.Machine, got.Title)
		}
	}
	check(store)

	// A later push updates the title and keeps the machine.
	store.UpdateSessionMeta("", []SessionMetaUpdate{{ID: "f9703c88", Title: "Renamed"}})
	if got := sessionsByID(store.Summary(10))["claude:f9703c88"]; got.Title != "Renamed" || got.Machine != "mbp-m3" {
		t.Fatalf("after rename title %q machine %q", got.Title, got.Machine)
	}
	store.UpdateSessionMeta("", []SessionMetaUpdate{{ID: "f9703c88", Title: "CLI proxy UI redesign"}})

	// Metadata survives a save and reload with the stats file.
	if errFlush := store.Flush(); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}
	reloaded := newClockStore(clock)
	reloaded.machineName = store.machineName
	reloaded.path = path
	reloaded.loadLocked()
	check(reloaded)

	// Metadata for sessions never seen is kept for 7 days, then dropped.
	reloaded.UpdateSessionMeta("mbp-m3", []SessionMetaUpdate{{ID: "unseen", Title: "Later"}})
	clock.now = clock.now.Add(6 * 24 * time.Hour)
	reloaded.Summary(10)
	if reloaded.meta["unseen"] == nil {
		t.Fatal("metadata dropped before 7 days")
	}
	clock.now = clock.now.Add(2 * 24 * time.Hour)
	reloaded.Summary(10)
	if reloaded.meta["unseen"] != nil {
		t.Fatal("metadata kept past 7 days")
	}
}

func TestHistogramPercentiles(t *testing.T) {
	var empty histogram
	if got := empty.percentile(0.5, ttftBase); got != 0 {
		t.Fatalf("empty p50 = %v, want 0", got)
	}

	var single histogram
	single.observe(1234, ttftBase)
	if got := single.percentile(0.99, ttftBase); math.Abs(got-1234)/1234 > 0.05 {
		t.Fatalf("single value p99 = %v, want within 5%% of 1234", got)
	}

	var uniform histogram
	for value := 1; value <= 1000; value++ {
		uniform.observe(float64(value), ttftBase)
	}
	for _, tc := range []struct{ p, want float64 }{{0.5, 500}, {0.9, 900}, {0.99, 990}} {
		if got := uniform.percentile(tc.p, ttftBase); math.Abs(got-tc.want)/tc.want > 0.06 {
			t.Fatalf("p%v = %v, want within 6%% of %v", tc.p*100, got, tc.want)
		}
	}

	// Values at or below the base fall in the first bucket; a value on a bound
	// stays in the bucket it closes.
	if histIndex(3, ttftBase) != 0 || histIndex(10, ttftBase) != 0 {
		t.Fatal("values up to the base must land in bucket 0")
	}
	if got := histIndex(histUpper(7, ttftBase), ttftBase); got != 7 {
		t.Fatalf("bound of bucket 7 landed in %d", got)
	}
	if histIndex(1e12, ttftBase) != histBuckets-1 {
		t.Fatal("huge values must clamp to the last bucket")
	}
	// Bucket bounds grow by about 10%.
	if ratio := histUpper(51, ttftBase) / histUpper(50, ttftBase); math.Abs(ratio-1.1) > 1e-9 {
		t.Fatalf("bucket growth = %v, want 1.1", ratio)
	}
}

func TestPerformanceSeriesBucketsPerRange(t *testing.T) {
	now := time.Date(2026, 10, 4, 15, 30, 0, 0, testZone)
	store := newTestStore(now)
	store.machineName = func(string) string { return "" }
	store.record(coreusage.Record{AuthID: "claude-a", Provider: "claude", RequestedAt: now, TTFT: time.Second, Latency: 3 * time.Second,
		Detail: coreusage.Detail{OutputTokens: 200}})
	store.record(coreusage.Record{AuthID: "codex-a", Provider: "codex", RequestedAt: now.Add(-23 * time.Hour), Failed: true})

	for _, tc := range []struct {
		key     string
		buckets int
		seconds int64
		first   time.Time
	}{
		{"24h", 24, 3600, time.Date(2026, 10, 3, 16, 0, 0, 0, testZone)},
		{"7d", 28, 21600, time.Date(2026, 9, 27, 18, 0, 0, 0, testZone)},
		{"14d", 28, 43200, time.Date(2026, 9, 21, 0, 0, 0, 0, testZone)},
	} {
		window, ok := LookupWindow(tc.key)
		if !ok {
			t.Fatalf("window %s missing", tc.key)
		}
		summary := store.SummaryFor(10, window)
		perf := summary.Performance
		if summary.Range != tc.key || perf.Range != tc.key || perf.BucketSeconds != tc.seconds {
			t.Fatalf("%s: range %q/%q bucket %d", tc.key, summary.Range, perf.Range, perf.BucketSeconds)
		}
		for _, scope := range []string{"all", "claude", "codex", "claude-a", "codex-a"} {
			series := perf.Scopes[scope].Series
			if len(series) != tc.buckets {
				t.Fatalf("%s/%s: %d buckets, want %d", tc.key, scope, len(series), tc.buckets)
			}
			if !series[0].Start.Equal(tc.first) {
				t.Fatalf("%s/%s: first bucket %s, want %s", tc.key, scope, series[0].Start, tc.first)
			}
			last := series[len(series)-1].Start
			if now.Before(last) || !now.Before(last.Add(time.Duration(tc.seconds)*time.Second)) {
				t.Fatalf("%s/%s: last bucket %s does not hold now", tc.key, scope, last)
			}
		}
		all := perf.Scopes["all"]
		if all.Requests != 2 || all.Failed != 1 || all.Series[tc.buckets-1].Requests != 1 {
			t.Fatalf("%s: all = %d requests %d failed, newest bucket %+v", tc.key, all.Requests, all.Failed, all.Series[tc.buckets-1])
		}
		if perf.Scopes["claude"].Requests != 1 || perf.Scopes["codex"].Failed != 1 {
			t.Fatalf("%s: provider scopes claude %+v codex %+v", tc.key, perf.Scopes["claude"], perf.Scopes["codex"])
		}
	}
	if _, ok := LookupWindow("90d"); ok {
		t.Fatal("unknown range accepted")
	}
}

func TestPerformanceThroughputAndFailovers(t *testing.T) {
	now := time.Date(2026, 10, 4, 15, 30, 0, 0, time.UTC)
	store := newTestStore(now)
	store.machineName = func(string) string { return "" }
	// Inbound request t1 failed on claude-a, then succeeded on claude-b.
	store.record(coreusage.Record{AuthID: "claude-a", Provider: "claude", TraceID: "t1", RequestedAt: now, Failed: true})
	store.record(coreusage.Record{AuthID: "claude-b", Provider: "claude", TraceID: "t1", RequestedAt: now, UpstreamStream: true,
		TTFT: time.Second, Latency: 3 * time.Second, Detail: coreusage.Detail{OutputTokens: 200}})
	// Inbound request t2 retried the same account: not a failover.
	store.record(coreusage.Record{AuthID: "claude-a", Provider: "claude", TraceID: "t2", RequestedAt: now, Failed: true})
	store.record(coreusage.Record{AuthID: "claude-a", Provider: "claude", TraceID: "t2", RequestedAt: now, UpstreamStream: true,
		TTFT: 2 * time.Second, Latency: 4 * time.Second, Detail: coreusage.Detail{OutputTokens: 10}})
	// A second success on t1 is not counted twice.
	store.record(coreusage.Record{AuthID: "claude-c", Provider: "claude", TraceID: "t1", RequestedAt: now})

	perf := store.Summary(10).Performance
	all := perf.Scopes["all"]
	if all.Failovers != 1 || perf.Scopes["claude"].Failovers != 1 || perf.Scopes["claude-b"].Failovers != 1 || perf.Scopes["claude-a"].Failovers != 0 {
		t.Fatalf("failovers all=%d claude=%d b=%d a=%d, want 1/1/1/0", all.Failovers, perf.Scopes["claude"].Failovers,
			perf.Scopes["claude-b"].Failovers, perf.Scopes["claude-a"].Failovers)
	}
	if all.Requests != 5 || all.Failed != 2 {
		t.Fatalf("all = %d requests %d failed, want 5 and 2", all.Requests, all.Failed)
	}
	// 200 tokens over 2 s of generation; the 10-token reply is too short to count.
	if math.Abs(all.Throughput.P50-100)/100 > 0.06 || len(all.ThroughputHist) != 1 || all.ThroughputHist[0].Count != 1 {
		t.Fatalf("throughput = %+v hist %+v, want one sample near 100", all.Throughput, all.ThroughputHist)
	}
	if len(all.TTFTHist) != 2 || all.TTFT.P99 < 1900 || all.TTFT.P50 > 1100 {
		t.Fatalf("ttft = %+v hist %+v", all.TTFT, all.TTFTHist)
	}
	if all.Latency.P50 < 2800 || all.Latency.P50 > 3200 {
		t.Fatalf("latency p50 = %d, want about 3000", all.Latency.P50)
	}
}

func TestPerformanceSurvivesReload(t *testing.T) {
	now := time.Date(2026, 10, 4, 15, 30, 0, 0, time.UTC)
	store := newTestStore(now)
	store.machineName = func(string) string { return "" }
	store.path = filepath.Join(t.TempDir(), "usage-stats.json")
	store.record(coreusage.Record{AuthID: "codex-a", Provider: "codex", RequestedAt: now, TTFT: time.Second, Latency: 2 * time.Second})
	if errFlush := store.Flush(); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}
	reloaded := newTestStore(now)
	reloaded.machineName = store.machineName
	reloaded.path = store.path
	reloaded.loadLocked()
	if got := reloaded.Summary(10).Performance.Scopes["codex"]; got.Requests != 1 || got.TTFT.P50 == 0 {
		t.Fatalf("reloaded codex scope = %+v", got)
	}
}
