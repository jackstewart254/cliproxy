package usagestats

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"
	"unsafe"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// sharesMemory reports whether stored points into source's bytes.
func sharesMemory(stored, source string) bool {
	if stored == "" || source == "" {
		return false
	}
	start := uintptr(unsafe.Pointer(unsafe.StringData(source)))
	at := uintptr(unsafe.Pointer(unsafe.StringData(stored)))
	return at >= start && at < start+uintptr(len(source))
}

func TestSessionMetaDoesNotKeepRequestStrings(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)}
	store := newClockStore(clock)
	padding := strings.Repeat(" ", 1<<16)
	title := strings.Repeat("t", 1<<20)
	id := "  s1" + padding
	machine := "mbp-m3" + padding

	if got := store.UpdateSessionMeta(machine, []SessionMetaUpdate{{ID: id, Title: title}}); got != 1 {
		t.Fatalf("updated = %d, want 1", got)
	}
	meta := store.meta["s1"]
	if meta == nil || len(meta.Title) != maxTitleLength || meta.Machine != "mbp-m3" {
		t.Fatalf("meta = %+v, want a clipped title and the trimmed machine", meta)
	}
	if sharesMemory(meta.Title, title) || sharesMemory(meta.Machine, machine) {
		t.Fatal("stored title or machine shares memory with the request")
	}
	for key := range store.meta {
		if sharesMemory(key, id) {
			t.Fatal("stored session id shares memory with the request")
		}
	}
}

func TestLoadDropsNilAuthCounts(t *testing.T) {
	now := time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	stamp := now.Add(-time.Minute).Format(time.RFC3339)
	data := fmt.Sprintf(`{"version":2,"hourly":{},"daily":{},"sessions":{"claude:s1":{"id":"wrong","by_auth":{"a":null,"b":{"requests":2,"failed":1}},"first_seen":%q,"last_seen":%q}}}`, stamp, stamp)
	if errWrite := os.WriteFile(path, []byte(data), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	store := newTestStore(now)
	store.machineName = func(string) string { return "" }
	store.path = path
	store.loadLocked()

	sessions := sessionsByID(store.Summary(10))
	got, ok := sessions["claude:s1"]
	if !ok {
		t.Fatalf("sessions = %+v, want claude:s1 under its map key", sessions)
	}
	if _, hasNil := got.ByAuth["a"]; hasNil || got.ByAuth["b"] == nil || got.ByAuth["b"].Requests != 2 {
		t.Fatalf("by_auth = %+v, want only b", got.ByAuth)
	}
}

func TestBucketsFollowLocalHoursInFractionalOffsetZone(t *testing.T) {
	india := time.FixedZone("IST", 5*3600+30*60)
	now := time.Date(2026, 10, 4, 10, 15, 0, 0, india)
	store := newTestStore(now)
	store.machineName = func(string) string { return "" }
	for _, at := range []time.Time{
		time.Date(2026, 10, 4, 10, 5, 0, 0, india),
		time.Date(2026, 10, 4, 6, 5, 0, 0, india),
		time.Date(2026, 10, 4, 0, 10, 0, 0, india),
	} {
		store.record(coreusage.Record{AuthID: "claude-a", Provider: "claude", RequestedAt: at})
	}

	day, _ := LookupWindow("24h")
	series := store.SummaryFor(10, day).Performance.Scopes["all"].Series
	if newest := series[23]; !newest.Start.Equal(time.Date(2026, 10, 4, 10, 0, 0, 0, india)) || newest.Requests != 1 {
		t.Fatalf("24h newest bucket = %+v, want the 10:05 request in the 10:00 bucket", newest)
	}
	if got := series[19]; got.Requests != 1 {
		t.Fatalf("24h 06:00 bucket = %+v, want the 06:05 request", got)
	}

	week, _ := LookupWindow("7d")
	series = store.SummaryFor(10, week).Performance.Scopes["all"].Series
	if series[27].Requests != 2 || series[26].Requests != 1 {
		t.Fatalf("7d buckets 06:00=%d 00:00=%d, want 2 and 1", series[27].Requests, series[26].Requests)
	}

	account := store.Summary(10).Accounts["claude-a"]
	if account.Today.Requests != 3 || account.Daily[13].Requests != 3 || account.Daily[12].Requests != 0 {
		t.Fatalf("today %d, daily today %d yesterday %d, want 3/3/0", account.Today.Requests, account.Daily[13].Requests, account.Daily[12].Requests)
	}
	if newest := account.Hourly[47]; !newest.Start.Equal(time.Date(2026, 10, 4, 10, 0, 0, 0, india)) || newest.Requests != 1 {
		t.Fatalf("newest hourly bucket = %+v, want the 10:05 request", newest)
	}
}

func TestBucketsAcrossDSTChanges(t *testing.T) {
	newYork, errZone := time.LoadLocation("America/New_York")
	if errZone != nil {
		t.Fatal(errZone)
	}
	day, _ := LookupWindow("24h")
	week, _ := LookupWindow("7d")

	// 1 Nov 2026: 01:00 to 02:00 happens twice. now is 01:30 the second time.
	now := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC).In(newYork)
	store := newTestStore(now)
	store.machineName = func(string) string { return "" }
	store.record(coreusage.Record{AuthID: "claude-a", Provider: "claude", RequestedAt: time.Date(2026, 11, 1, 6, 10, 0, 0, time.UTC)})
	store.record(coreusage.Record{AuthID: "claude-a", Provider: "claude", RequestedAt: time.Date(2026, 11, 1, 5, 10, 0, 0, time.UTC)})
	store.record(coreusage.Record{AuthID: "claude-a", Provider: "claude", RequestedAt: time.Date(2026, 11, 1, 4, 10, 0, 0, time.UTC)})

	series := store.SummaryFor(10, day).Performance.Scopes["all"].Series
	if newest := series[23]; !newest.Start.Equal(time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)) || newest.Requests != 1 {
		t.Fatalf("fall back: newest hour = %+v, want the second 01:00 with one request", newest)
	}
	if series[22].Requests != 1 || series[21].Requests != 1 {
		t.Fatalf("fall back: earlier hours %d and %d, want 1 and 1", series[22].Requests, series[21].Requests)
	}
	series = store.SummaryFor(10, week).Performance.Scopes["all"].Series
	if newest := series[27]; !newest.Start.Equal(time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)) || newest.Requests != 3 {
		t.Fatalf("fall back: newest 6h bucket = %+v, want local midnight with three requests", newest)
	}
	if got := store.Summary(10).Accounts["claude-a"]; got.Today.Requests != 3 || got.Hourly[47].Requests != 1 {
		t.Fatalf("fall back: today %d newest hour %d, want 3 and 1", got.Today.Requests, got.Hourly[47].Requests)
	}

	// 8 Mar 2026: 02:00 to 03:00 is skipped. now is 03:30.
	now = time.Date(2026, 3, 8, 7, 30, 0, 0, time.UTC).In(newYork)
	store = newTestStore(now)
	store.machineName = func(string) string { return "" }
	store.record(coreusage.Record{AuthID: "claude-a", Provider: "claude", RequestedAt: time.Date(2026, 3, 8, 7, 5, 0, 0, time.UTC)})
	store.record(coreusage.Record{AuthID: "claude-a", Provider: "claude", RequestedAt: time.Date(2026, 3, 8, 6, 5, 0, 0, time.UTC)})
	series = store.SummaryFor(10, day).Performance.Scopes["all"].Series
	if series[23].Requests != 1 || series[22].Requests != 1 || !series[22].Start.Equal(time.Date(2026, 3, 8, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("spring forward: hours %+v and %+v, want 01:00 and 03:00 with one request each", series[22], series[23])
	}
	series = store.SummaryFor(10, week).Performance.Scopes["all"].Series
	if newest := series[27]; !newest.Start.Equal(time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)) || newest.Requests != 2 {
		t.Fatalf("spring forward: newest 6h bucket = %+v, want local midnight with two requests", newest)
	}
}

func TestThroughputCountsOnlyStreamedReplies(t *testing.T) {
	now := time.Date(2026, 10, 4, 15, 30, 0, 0, time.UTC)
	store := newTestStore(now)
	store.machineName = func(string) string { return "" }
	// A buffered upstream reply: its first byte came after the whole body was
	// ready, even though the client asked for a stream.
	store.record(coreusage.Record{AuthID: "claude-a", Provider: "claude", RequestedAt: now, Stream: true,
		TTFT: 2900 * time.Millisecond, Latency: 3 * time.Second, Detail: coreusage.Detail{OutputTokens: 400}})
	// Streamed from upstream to answer a client that did not ask for a stream.
	store.record(coreusage.Record{AuthID: "claude-a", Provider: "claude", RequestedAt: now, UpstreamStream: true,
		TTFT: time.Second, Latency: 3 * time.Second, Detail: coreusage.Detail{OutputTokens: 200}})

	all := store.Summary(10).Performance.Scopes["all"]
	if len(all.ThroughputHist) != 1 || all.ThroughputHist[0].Count != 1 || all.Throughput.P50 > 110 {
		t.Fatalf("throughput = %+v hist %+v, want only the streamed reply near 100", all.Throughput, all.ThroughputHist)
	}
	if len(all.TTFTHist) != 2 {
		t.Fatalf("ttft hist = %+v, want both replies", all.TTFTHist)
	}
}

func TestTracesKeepActiveRetryChains(t *testing.T) {
	now := time.Date(2026, 10, 4, 15, 30, 0, 0, time.UTC)
	store := newTestStore(now)
	store.machineName = func(string) string { return "" }
	fail := func(trace, auth string, at time.Time) {
		store.record(coreusage.Record{AuthID: auth, Provider: "claude", TraceID: trace, RequestedAt: at, Failed: true})
	}
	succeed := func(trace, auth string, at time.Time) {
		store.record(coreusage.Record{AuthID: auth, Provider: "claude", TraceID: trace, RequestedAt: at})
	}

	// Completed chains free their trace.
	for i := 0; i < maxTraces-1; i++ {
		trace := fmt.Sprintf("done-%d", i)
		fail(trace, "claude-a", now)
		succeed(trace, "claude-b", now)
	}
	if len(store.traces) != 0 {
		t.Fatalf("%d traces left after every chain completed, want 0", len(store.traces))
	}
	fail("A", "claude-a", now)
	fail("B", "claude-a", now)
	succeed("A", "claude-b", now)
	if got := store.Summary(10).Performance.Scopes["all"].Failovers; got != maxTraces {
		t.Fatalf("failovers = %d, want %d", got, maxTraces)
	}

	// A full map of active chains loses only its oldest one.
	store = newTestStore(now)
	store.machineName = func(string) string { return "" }
	for i := 0; i < maxTraces; i++ {
		fail(fmt.Sprintf("open-%d", i), "claude-a", now.Add(-time.Duration(maxTraces-i)*time.Millisecond))
	}
	fail("newest", "claude-a", now)
	if len(store.traces) != maxTraces || store.traces["open-0"] != nil || store.traces["open-1"] == nil {
		t.Fatalf("after overflow: %d traces, oldest kept=%v, second kept=%v", len(store.traces), store.traces["open-0"] != nil, store.traces["open-1"] != nil)
	}
	succeed("open-1", "claude-b", now)
	succeed("newest", "claude-b", now)
	if got := store.Summary(10).Performance.Scopes["all"].Failovers; got != 2 {
		t.Fatalf("failovers = %d, want 2", got)
	}
}

func TestSessionFieldsFollowTheLatestRequest(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)}
	store := newClockStore(clock)
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	store.path = path
	newer := clock.now.Add(-time.Minute)
	older := clock.now.Add(-2 * time.Minute)

	// The newer request finishes first; the older, slower one finishes after it.
	store.recordFrom(coreusage.Record{AuthID: "claude-b", SessionID: "claude:s1", Model: "claude-opus-5-5", RequestedAt: newer}, "192.0.2.2")
	store.recordFrom(coreusage.Record{AuthID: "claude-a", SessionID: "claude:s1", Model: "claude-sonnet-5-5", RequestedAt: older}, "192.0.2.1")
	check := func(store *Store, wantModel string, wantSeen time.Time) {
		t.Helper()
		session := store.sessions["claude:s1"]
		if session.ServingAuthID != "claude-b" || session.Model != wantModel || session.ClientIP != "192.0.2.2" || !session.LastSeen.Equal(wantSeen) {
			t.Fatalf("session serving %q model %q ip %q last seen %s", session.ServingAuthID, session.Model, session.ClientIP, session.LastSeen)
		}
	}
	check(store, "claude-opus-5-5", newer)

	// A newest attempt that failed moves the model and last seen, not the
	// serving account; a late success from before it does not move it back.
	latest := clock.now.Add(-30 * time.Second)
	store.recordFrom(coreusage.Record{AuthID: "claude-c", SessionID: "claude:s1", Model: "claude-fable-5-1", RequestedAt: latest, Failed: true}, "192.0.2.2")
	store.recordFrom(coreusage.Record{AuthID: "claude-a", SessionID: "claude:s1", Model: "claude-sonnet-5-5", RequestedAt: older.Add(time.Second)}, "192.0.2.1")
	check(store, "claude-fable-5-1", latest)

	// The dashboard folds agent threads into their parent and needs
	// served_at to pick which one's serving account is current.
	view := sessionsByID(store.Summary(10))["claude:s1"]
	if !view.ServedAt.Equal(newer) {
		t.Fatalf("summary served_at = %v, want %v", view.ServedAt, newer)
	}
	if errFlush := store.Flush(); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}
	reloaded := newClockStore(clock)
	reloaded.path = path
	reloaded.loadLocked()
	reloaded.recordFrom(coreusage.Record{AuthID: "claude-a", SessionID: "claude:s1", RequestedAt: older.Add(2 * time.Second)}, "")
	check(reloaded, "claude-fable-5-1", latest)
}

// Every hub request arrives over Tailscale Serve from loopback, so the client
// key's device names the session's machine instead of the hub itself. The key
// is never stored, and a key without a device keeps the loopback fallback.
func TestSessionMachineFromDeviceClientKey(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	store := newClockStore(clock)
	store.machineName = func(string) string { return "jacks-desktop" }
	at := clock.now.Add(-time.Minute)
	for id, key := range map[string]string{
		"claude:air":   "dev.macbook-air.0123456789abcdef0123456789abcdef",
		"claude:local": "plain-client-key",
		"claude:bad":   "dev.Not_A_Name.0123456789abcdef",
	} {
		store.recordFrom(coreusage.Record{AuthID: "claude-a", SessionID: id, APIKey: key, RequestedAt: at}, "127.0.0.1")
	}
	sessions := sessionsByID(store.Summary(10))
	for id, want := range map[string]string{"claude:air": "macbook-air", "claude:local": "jacks-desktop", "claude:bad": "jacks-desktop"} {
		if got := sessions[id].Machine; got != want {
			t.Errorf("%s machine = %q, want %q", id, got, want)
		}
		if sessions[id].Device != "" {
			t.Errorf("%s summary leaks device field %q", id, sessions[id].Device)
		}
	}
	if got := store.sessions["claude:air"].Device; got != "macbook-air" {
		t.Fatalf("stored device = %q, want macbook-air", got)
	}
}
