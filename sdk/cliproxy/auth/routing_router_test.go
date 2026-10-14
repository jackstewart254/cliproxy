package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var routerTestNow = time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)

func newTestRoutingState(now time.Time) *RoutingState {
	state := NewRoutingState()
	state.nowFunc = func() time.Time { return now }
	return state
}

func routerTestAuth(id, provider, email string) *Auth {
	return &Auth{ID: id, Provider: provider, Metadata: map[string]any{"email": email}}
}

// setMeter stores a meter reading for authID as if a response had reported it.
func setMeter(state *RoutingState, authID, provider string, meter Meter) {
	state.mu.Lock()
	defer state.mu.Unlock()
	account := state.accounts[authID]
	if account == nil {
		account = &accountRouting{Provider: provider, Meters: map[string]*Meter{}, History: map[string][]MeterSample{}}
		state.accounts[authID] = account
	}
	if meter.ObservedAt.IsZero() {
		meter.ObservedAt = routerTestNow.Add(-time.Minute)
	}
	if meter.Window == 0 {
		meter.Window = claudeClaimWindow(meter.Name)
	}
	copyMeter := meter
	account.Meters[meter.Name] = &copyMeter
}

func unixText(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func TestParseClaudeMetersReadsEveryClaim(t *testing.T) {
	headers := http.Header{}
	headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.26")
	headers.Set("Anthropic-Ratelimit-Unified-5h-Reset", unixText(routerTestNow.Add(4*time.Hour)))
	headers.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.08")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Reset", unixText(routerTestNow.Add(6*24*time.Hour)))
	headers.Set("Anthropic-Ratelimit-Unified-7d_fable-Utilization", "0.4")
	headers.Set("Anthropic-Ratelimit-Unified-7d_fable-Reset", unixText(routerTestNow.Add(3*24*time.Hour)))
	headers.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
	headers.Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
	headers.Set("Anthropic-Ratelimit-Unified-Status", "allowed")

	meters, facts := parseMeters("claude", headers, routerTestNow)
	if len(meters) != 3 {
		t.Fatalf("meters = %v, want 5h, 7d and 7d_fable", meters)
	}
	if got := meters["5h"]; got.Utilization != 0.26 || got.Window != fiveHourWindow || got.Status != "allowed" {
		t.Fatalf("5h meter = %+v", got)
	}
	if got := meters["7d_fable"]; got.Utilization != 0.4 || got.Window != weeklyQuotaWindow || !got.ResetAt.Equal(routerTestNow.Add(3*24*time.Hour)) {
		t.Fatalf("7d_fable meter = %+v", got)
	}
	if facts.BindingClaim != "five_hour" || facts.OverageStatus != "rejected" || facts.Status != "allowed" {
		t.Fatalf("facts = %+v", facts)
	}
}

func TestParseCodexMetersReadsWindowsAndNamedLimits(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Codex-Primary-Used-Percent", "12")
	headers.Set("X-Codex-Primary-Window-Minutes", "300")
	headers.Set("X-Codex-Primary-Reset-After-Seconds", "3600")
	headers.Set("X-Codex-Secondary-Used-Percent", "40")
	headers.Set("X-Codex-Secondary-Window-Minutes", "10080")
	headers.Set("X-Codex-Secondary-Reset-At", unixText(routerTestNow.Add(48*time.Hour)))
	headers.Set("X-Codex-Bengalfox-Secondary-Used-Percent", "70")
	headers.Set("X-Codex-Bengalfox-Limit-Name", "GPT-6 Astra")
	headers.Set("X-Codex-Active-Limit", "codex")

	meters, facts := parseMeters("codex", headers, routerTestNow)
	if got := meters["primary"]; got == nil || got.Utilization != 0.12 || got.Window != 5*time.Hour || !got.ResetAt.Equal(routerTestNow.Add(time.Hour)) {
		t.Fatalf("primary = %+v", got)
	}
	if got := meters["secondary"]; got == nil || got.Utilization != 0.4 || !meterIsLong(got) {
		t.Fatalf("secondary = %+v", got)
	}
	if got := meters["bengalfox-secondary"]; got == nil || got.Utilization != 0.7 || got.Label != "GPT-6 Astra" {
		t.Fatalf("named limit = %+v", got)
	}
	if facts.ActiveLimit != "codex" {
		t.Fatalf("facts = %+v", facts)
	}
	if meterAppliesToModel("codex", meters["bengalfox-secondary"], "gpt-6-astra") {
		t.Fatal("named Codex limits are shown, not applied")
	}
}

func TestObserveResultKeepsModelMeterAfterResponseWithoutIt(t *testing.T) {
	state := newTestRoutingState(routerTestNow)
	fable := http.Header{}
	fable.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.1")
	fable.Set("Anthropic-Ratelimit-Unified-7d_fable-Utilization", "0.5")
	state.observeResult(Result{AuthID: "a", Provider: "claude", Model: "claude-fable-5-1", Success: true}, fable, routerTestNow)

	opus := http.Header{}
	opus.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.12")
	state.observeResult(Result{AuthID: "a", Provider: "claude", Model: "claude-opus-5-5", Success: true}, opus, routerTestNow.Add(time.Minute))

	account := state.account("a")
	if got := account.Meters["7d_fable"]; got == nil || got.Utilization != 0.5 {
		t.Fatalf("7d_fable = %+v, want it kept from the earlier response", got)
	}
	if got := account.Meters["7d"]; got.Utilization != 0.12 || len(got.Models) != 2 {
		t.Fatalf("7d = %+v, want the newest value and both models", got)
	}
	if account.Facts.LastModel != "claude-opus-5-5" {
		t.Fatalf("last model = %q", account.Facts.LastModel)
	}
}

func TestObserveResultSkipsCountTokensMetersButRecordsHealth(t *testing.T) {
	state := newTestRoutingState(routerTestNow)
	headers := http.Header{}
	headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.5")
	state.observeResult(Result{
		AuthID: "a", Provider: "claude", SkipQuotaObservation: true,
		Error: &Error{HTTPStatus: http.StatusForbidden, Message: `{"type":"error","error":{"type":"permission_error","message":"OAuth authentication is currently not allowed for this organization.","details":{"error_code":"oauth_not_allowed_for_organization"}}}`},
	}, headers, routerTestNow)

	account := state.account("a")
	if len(account.Meters) != 0 {
		t.Fatalf("meters = %v, want none from a count-tokens result", account.Meters)
	}
	reason, refused := account.Health.refusedReason(routerTestNow.Add(time.Hour))
	if !refused || !strings.Contains(reason, "oauth_not_allowed_for_organization") {
		t.Fatalf("refused = %v %q", refused, reason)
	}
	if _, refused = account.Health.refusedReason(routerTestNow.Add(refusalQuarantine)); refused {
		t.Fatal("refusal should lapse after the quarantine")
	}
}

func TestRefusalClearsAfterSuccessAndIgnoresOtherFailures(t *testing.T) {
	health := AccountHealth{LastFailureAt: routerTestNow, LastFailureStatus: http.StatusForbidden, LastFailureCode: "permission_error"}
	if _, refused := health.refusedReason(routerTestNow); !refused {
		t.Fatal("a 403 permission error should count as a refusal")
	}
	health.LastSuccessAt = routerTestNow.Add(time.Second)
	if _, refused := health.refusedReason(routerTestNow.Add(time.Minute)); refused {
		t.Fatal("a later success should clear the refusal")
	}
	for _, failure := range []AccountHealth{
		{LastFailureAt: routerTestNow, LastFailureStatus: http.StatusTooManyRequests, LastFailureCode: "rate_limit_error"},
		{LastFailureAt: routerTestNow, LastFailureStatus: http.StatusUnauthorized, LastFailureCode: "authentication_error"},
		{LastFailureAt: routerTestNow, LastFailureStatus: http.StatusForbidden, LastFailureCode: "cloudflare"},
	} {
		if _, refused := failure.refusedReason(routerTestNow); refused {
			t.Fatalf("%+v should not count as a refusal", failure)
		}
	}
}

func TestMeterAppliesToModel(t *testing.T) {
	week := &Meter{Name: "7d"}
	fable := &Meter{Name: "7d_fable"}
	other := &Meter{Name: "7d_oauth_apps", Models: []string{"claude-opus-5-5"}}
	if !meterAppliesToModel("claude", week, "claude-haiku-4-5") {
		t.Fatal("the shared week covers every model")
	}
	if !meterAppliesToModel("claude", fable, "claude-fable-5-1") || meterAppliesToModel("claude", fable, "claude-opus-5-5") {
		t.Fatal("a family claim covers only that family")
	}
	if !meterAppliesToModel("claude", other, "claude-opus-5") || meterAppliesToModel("claude", other, "claude-sonnet-5") {
		t.Fatal("an unknown claim covers the families it was reported for")
	}
}

func TestEffectiveMeterRollsPassedResetForward(t *testing.T) {
	meter := Meter{Name: "7d", Utilization: 0.9, Window: weeklyQuotaWindow, ResetAt: routerTestNow.Add(-time.Hour), Status: "rejected"}
	effective, reset := effectiveMeter(meter, routerTestNow)
	if !reset || effective.Utilization != 0 || effective.Status != "" || !effective.ResetAt.Equal(routerTestNow.Add(weeklyQuotaWindow-time.Hour)) {
		t.Fatalf("effective = %+v reset=%v", effective, reset)
	}
}

func TestBurnRateUsesSamplesSinceTheLastReset(t *testing.T) {
	samples := []MeterSample{
		{At: routerTestNow.Add(-80 * time.Minute), Utilization: 0.9},
		{At: routerTestNow.Add(-60 * time.Minute), Utilization: 0.05},
		{At: routerTestNow, Utilization: 0.25},
	}
	rate, known := burnRate(samples, routerTestNow, burnLookback)
	if !known || rate < 0.199 || rate > 0.201 {
		t.Fatalf("rate = %v known=%v, want 0.2 an hour after the reset", rate, known)
	}
	if _, known = burnRate(samples[2:], routerTestNow, burnLookback); known {
		t.Fatal("one sample is not enough history")
	}
}

// TestBurnRateIgnoresOutOfOrderReadings covers replies that finish out of
// order: a reading one point below the one before is not a reset, so the rate
// still spans the whole hour.
func TestBurnRateIgnoresOutOfOrderReadings(t *testing.T) {
	samples := []MeterSample{
		{At: routerTestNow.Add(-60 * time.Minute), Utilization: 0.40},
		{At: routerTestNow.Add(-40 * time.Minute), Utilization: 0.42},
		{At: routerTestNow.Add(-39 * time.Minute), Utilization: 0.41},
		{At: routerTestNow.Add(-5 * time.Minute), Utilization: 0.50},
		{At: routerTestNow, Utilization: 0.49},
	}
	rate, known := burnRate(samples, routerTestNow, burnLookback)
	if !known || rate < 0.099 || rate > 0.101 {
		t.Fatalf("rate = %v known=%v, want 0.1 an hour from 0.40 to the 0.50 high", rate, known)
	}
}

// TestWeeklyBurnUsesLongerLookback covers a weekly meter that moves one point
// every two hours: 90 minutes of samples show no rise, six hours do.
func TestWeeklyBurnUsesLongerLookback(t *testing.T) {
	state := newTestRoutingState(routerTestNow)
	seat := routerTestAuth("a-seat", "claude", "seat@example.com")
	setMeter(state, seat.ID, "claude", Meter{Name: "7d", Utilization: 0.20, ResetAt: routerTestNow.Add(5 * 24 * time.Hour)})
	setMeter(state, seat.ID, "claude", Meter{Name: "5h", Utilization: 0.30, ResetAt: routerTestNow.Add(2 * time.Hour)})
	var weekly, short []MeterSample
	for minutes := 360; minutes >= 0; minutes -= 10 {
		at := routerTestNow.Add(-time.Duration(minutes) * time.Minute)
		weekly = append(weekly, MeterSample{At: at, Utilization: 0.17 + float64((360-minutes)/120)*0.01})
		short = append(short, MeterSample{At: at, Utilization: 0.30})
	}
	state.accounts[seat.ID].History["7d"] = weekly
	state.accounts[seat.ID].History["5h"] = short

	view := state.Dashboard([]*Auth{seat}, routerTestNow)
	for _, meter := range view.Accounts[seat.ID].Meters {
		burn := -1.0
		if meter.BurnPerHour != nil {
			burn = *meter.BurnPerHour
		}
		switch meter.Name {
		case "7d":
			if burn < 0.004 || burn > 0.006 {
				t.Fatalf("weekly burn = %v, want about 0.005 an hour over six hours", burn)
			}
		case "5h":
			if burn != 0 {
				t.Fatalf("5-hour burn = %v, want 0 over the last 90 minutes", burn)
			}
		}
	}
}

// TestScoredSelectorPrefersAllowanceAtRisk is the case soonest-reset gets
// wrong: 3% left that resets in 6 hours is worth less than 80% left that
// resets in 2 days.
func TestScoredSelectorPrefersAllowanceAtRisk(t *testing.T) {
	state := newTestRoutingState(routerTestNow)
	soon := routerTestAuth("a-soon", "claude", "soon@example.com")
	later := routerTestAuth("b-later", "claude", "later@example.com")
	setMeter(state, soon.ID, "claude", Meter{Name: "7d", Utilization: 0.97, ResetAt: routerTestNow.Add(6 * time.Hour)})
	setMeter(state, later.ID, "claude", Meter{Name: "7d", Utilization: 0.2, ResetAt: routerTestNow.Add(48 * time.Hour)})
	for _, auth := range []*Auth{soon, later} {
		auth.Quota.Signals = map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixText(routerTestNow.Add(6 * time.Hour))}
	}
	later.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = unixText(routerTestNow.Add(48 * time.Hour))

	scorer := NewScoredSelector(ScorerConfig{}, state)
	scorer.nowFunc = func() time.Time { return routerTestNow }
	picked, err := scorer.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{}, []*Auth{soon, later})
	if err != nil || picked.ID != later.ID {
		t.Fatalf("scored pick = %v, %v; want %s", picked, err, later.ID)
	}

	live := &SoonestResetSelector{nowFunc: func() time.Time { return routerTestNow }, state: state}
	livePick, _ := live.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{}, []*Auth{soon, later})
	if livePick.ID != soon.ID {
		t.Fatalf("soonest-reset pick = %s, want %s (the contrast this test documents)", livePick.ID, soon.ID)
	}
}

func TestScoredSelectorDemotesAccountAboveOwnerHeadroom(t *testing.T) {
	state := newTestRoutingState(routerTestNow)
	seat := routerTestAuth("a-seat", "claude", "Seat@Example.com")
	other := routerTestAuth("b-other", "claude", "other@example.com")
	setMeter(state, seat.ID, "claude", Meter{Name: "7d", Utilization: 0.1, ResetAt: routerTestNow.Add(24 * time.Hour)})
	setMeter(state, seat.ID, "claude", Meter{Name: "5h", Utilization: 0.75, ResetAt: routerTestNow.Add(4 * time.Hour)})
	setMeter(state, other.ID, "claude", Meter{Name: "7d", Utilization: 0.1, ResetAt: routerTestNow.Add(100 * time.Hour)})

	scorer := NewScoredSelector(ScorerConfig{Headroom: map[string]float64{"seat@example.com": 0.7}}, state)
	ranking := scorer.rankAvailable("claude-opus-5-5", []*Auth{seat, other}, routerTestNow)
	if ranking[0].AuthID != other.ID {
		t.Fatalf("ranking = %+v, want the seat above its line ranked second", ranking)
	}
	if len(ranking[1].Problems) != 1 || !strings.Contains(ranking[1].Problems[0], "70% line") {
		t.Fatalf("seat problems = %v", ranking[1].Problems)
	}
}

func TestScoredSelectorFlagsFiveHourWindowOnCourseToRunOut(t *testing.T) {
	state := newTestRoutingState(routerTestNow)
	busy := routerTestAuth("a-busy", "claude", "busy@example.com")
	setMeter(state, busy.ID, "claude", Meter{Name: "7d", Utilization: 0.1, ResetAt: routerTestNow.Add(24 * time.Hour)})
	setMeter(state, busy.ID, "claude", Meter{Name: "5h", Utilization: 0.5, ResetAt: routerTestNow.Add(3 * time.Hour)})
	state.accounts[busy.ID].History["5h"] = []MeterSample{
		{At: routerTestNow.Add(-time.Hour), Utilization: 0.3},
		{At: routerTestNow, Utilization: 0.5},
	}
	candidate := NewScoredSelector(ScorerConfig{}, state).assess(busy, "claude-opus-5-5", 0, routerTestNow)
	if len(candidate.Problems) != 1 || !strings.HasPrefix(candidate.Problems[0], "5-hour window on course to run out") {
		t.Fatalf("problems = %v, want the 5-hour projection flagged (0.5 + 0.2/h x 3h = 1.1)", candidate.Problems)
	}
	if !strings.Contains(candidate.Summary, "rising 20% an hour") {
		t.Fatalf("summary = %q", candidate.Summary)
	}
}

func TestScoredSelectorExcludesFullAndRefusedAccounts(t *testing.T) {
	state := newTestRoutingState(routerTestNow)
	full := routerTestAuth("a-full", "claude", "full@example.com")
	refused := routerTestAuth("b-refused", "claude", "refused@example.com")
	setMeter(state, full.ID, "claude", Meter{Name: "7d_fable", Utilization: 1, ResetAt: routerTestNow.Add(24 * time.Hour)})
	setMeter(state, full.ID, "claude", Meter{Name: "7d", Utilization: 0.3, ResetAt: routerTestNow.Add(24 * time.Hour)})
	state.accounts[refused.ID] = &accountRouting{Provider: "claude", Health: AccountHealth{
		LastFailureAt: routerTestNow.Add(-time.Minute), LastFailureStatus: http.StatusForbidden, LastFailureCode: "oauth_not_allowed_for_organization",
	}}
	scorer := NewScoredSelector(ScorerConfig{}, state)

	fable := scorer.rankAvailable("claude-fable-5-1", []*Auth{full, refused}, routerTestNow)
	for _, candidate := range fable {
		if !candidate.Excluded {
			t.Fatalf("candidate %+v should be excluded for Fable", candidate)
		}
	}
	if !strings.Contains(fable[0].Summary+fable[1].Summary, "Fable week used up") {
		t.Fatalf("summaries = %q / %q", fable[0].Summary, fable[1].Summary)
	}
	if got := bestCandidate([]*Auth{full, refused}, fable); got.ID != full.ID {
		t.Fatalf("with every candidate excluded the first available should be used, got %s", got.ID)
	}

	opus := scorer.rankAvailable("claude-opus-5-5", []*Auth{full, refused}, routerTestNow)
	if opus[0].AuthID != full.ID || opus[0].Excluded {
		t.Fatalf("the Fable meter must not block Opus: %+v", opus)
	}
}

func TestScoredSelectorRankExplainsOffAndCoolingAccounts(t *testing.T) {
	state := newTestRoutingState(routerTestNow)
	ready := routerTestAuth("a-ready", "claude", "ready@example.com")
	off := routerTestAuth("b-off", "claude", "off@example.com")
	off.Disabled = true
	cooling := routerTestAuth("c-cooling", "claude", "cooling@example.com")
	cooling.Unavailable = true
	cooling.NextRetryAfter = routerTestNow.Add(2 * time.Hour)

	ranking := NewScoredSelector(ScorerConfig{}, state).Rank("claude-opus-5-5", []*Auth{ready, off, cooling}, routerTestNow)
	if len(ranking) != 3 || ranking[0].AuthID != ready.ID || ranking[0].Excluded {
		t.Fatalf("ranking = %+v", ranking)
	}
	if ranking[1].Summary != "Off" || !strings.HasPrefix(ranking[2].Summary, "Cooling down until") {
		t.Fatalf("explanations = %q / %q", ranking[1].Summary, ranking[2].Summary)
	}
}

func TestShadowSelectorKeepsLivePickAndRecordsComparison(t *testing.T) {
	state := newTestRoutingState(routerTestNow)
	soon := routerTestAuth("a-soon", "claude", "soon@example.com")
	later := routerTestAuth("b-later", "claude", "later@example.com")
	soon.Quota.Signals = map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixText(routerTestNow.Add(6 * time.Hour))}
	later.Quota.Signals = map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixText(routerTestNow.Add(48 * time.Hour))}
	setMeter(state, soon.ID, "claude", Meter{Name: "7d", Utilization: 0.97, ResetAt: routerTestNow.Add(6 * time.Hour)})
	setMeter(state, later.ID, "claude", Meter{Name: "7d", Utilization: 0.2, ResetAt: routerTestNow.Add(48 * time.Hour)})

	scorer := NewScoredSelector(ScorerConfig{}, state)
	scorer.nowFunc = func() time.Time { return routerTestNow }
	live := &SoonestResetSelector{nowFunc: func() time.Time { return routerTestNow }, state: state}
	shadow := NewShadowSelector(live, scorer, state)
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.CanonicalSessionIDMetadataKey: "claude:session-1"}}

	picked, err := shadow.Pick(context.Background(), "mixed", "claude-opus-5-5", opts, []*Auth{soon, later})
	if err != nil || picked.ID != soon.ID {
		t.Fatalf("shadow mode must return the live pick, got %v, %v", picked, err)
	}
	summary := state.shadowSummary(routerTestNow, 10)
	if summary.Choices != 1 || summary.Agreed != 0 || len(summary.Disagreed) != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	decision := summary.Disagreed[0]
	if decision.Provider != "claude" || decision.Live != soon.ID || decision.Shadow != later.ID || decision.Session != "claude:session-1" || len(decision.Ranking) != 2 {
		t.Fatalf("decision = %+v", decision)
	}

	// A placement with only one candidate is not a choice and is not recorded.
	if _, err = shadow.Pick(context.Background(), "claude", "claude-opus-5-5", opts, []*Auth{soon}); err != nil {
		t.Fatal(err)
	}
	if got := state.shadowSummary(routerTestNow, 10).Choices; got != 1 {
		t.Fatalf("choices = %d, want 1", got)
	}
}

func TestSoonestResetSkipsRefusedAccount(t *testing.T) {
	state := newTestRoutingState(routerTestNow)
	refused := routerTestAuth("a-refused", "claude", "refused@example.com")
	healthy := routerTestAuth("b-healthy", "claude", "healthy@example.com")
	state.accounts[refused.ID] = &accountRouting{Health: AccountHealth{
		LastFailureAt: routerTestNow.Add(-time.Minute), LastFailureStatus: http.StatusForbidden, LastFailureCode: "oauth_not_allowed_for_organization",
	}}
	live := &SoonestResetSelector{nowFunc: func() time.Time { return routerTestNow }, state: state}
	picked, err := live.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{}, []*Auth{refused, healthy})
	if err != nil || picked.ID != healthy.ID {
		t.Fatalf("picked %v, %v; want the healthy account", picked, err)
	}
	// With nothing else left the refused account is still used.
	picked, _ = live.Pick(context.Background(), "claude", "claude-opus-5-5", cliproxyexecutor.Options{}, []*Auth{refused})
	if picked.ID != refused.ID {
		t.Fatalf("picked %s, want the only account", picked.ID)
	}
}

func TestSessionCacheSnapshotRestoreAndActiveCounts(t *testing.T) {
	first := NewSessionCache(time.Hour)
	defer first.Stop()
	first.Set("claude::s1::claude-opus-5-5", "auth-a")
	first.Set("claude::s1::claude-haiku-4-5", "auth-a")
	first.Set("claude::s2::claude-opus-5-5", "auth-b")

	if counts := first.ActiveSessionsByAuth(time.Now().Add(-time.Minute)); counts["auth-a"] != 1 || counts["auth-b"] != 1 {
		t.Fatalf("active = %v, want one session each (per-model bindings count once)", counts)
	}

	second := NewSessionCache(time.Hour)
	defer second.Stop()
	second.Set("claude::s2::claude-opus-5-5", "auth-c")
	second.Restore(first.Snapshot())
	if got, _ := second.Get("claude::s1::claude-opus-5-5"); got != "auth-a" {
		t.Fatalf("restored binding = %q, want auth-a", got)
	}
	if got, _ := second.Get("claude::s2::claude-opus-5-5"); got != "auth-c" {
		t.Fatalf("live binding = %q, want it kept over the restored one", got)
	}
}

func TestRoutingStatePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routing-state.json")
	first := newTestRoutingState(routerTestNow)
	first.path = path
	cache := NewSessionCache(time.Hour)
	defer cache.Stop()
	first.AttachSessionCache(cache)
	cache.Set("claude::s1::claude-opus-5-5", "auth-a")
	headers := http.Header{}
	headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.3")
	first.observeResult(Result{AuthID: "auth-a", Provider: "claude", Model: "claude-opus-5-5", Success: true}, headers, routerTestNow)
	first.recordDecision(ShadowDecision{At: routerTestNow, Provider: "claude", Live: "auth-a", Shadow: "auth-a"})
	if errFlush := first.Flush(); errFlush != nil {
		t.Fatalf("Flush() error = %v", errFlush)
	}

	second := newTestRoutingState(routerTestNow)
	second.path = path
	second.mu.Lock()
	second.loadLocked()
	second.mu.Unlock()
	if got := second.account("auth-a"); got == nil || got.Meters["7d"].Utilization != 0.3 {
		t.Fatalf("reloaded account = %+v", got)
	}
	if got := second.shadowSummary(routerTestNow, 10).Choices; got != 1 {
		t.Fatalf("reloaded decisions = %d", got)
	}
	restored := NewSessionCache(time.Hour)
	defer restored.Stop()
	second.AttachSessionCache(restored)
	if got, _ := restored.Get("claude::s1::claude-opus-5-5"); got != "auth-a" {
		t.Fatalf("restored binding = %q, want auth-a", got)
	}
}

func TestRoutingDashboardShowsMetersRankingAndShadow(t *testing.T) {
	state := newTestRoutingState(routerTestNow)
	state.SetScorer("shadow", ScorerConfig{Headroom: map[string]float64{"seat@example.com": 0.7}})
	seat := routerTestAuth("a-seat", "claude", "seat@example.com")
	setMeter(state, seat.ID, "claude", Meter{Name: "5h", Utilization: 0.26, ResetAt: routerTestNow.Add(2 * time.Hour)})
	setMeter(state, seat.ID, "claude", Meter{Name: "7d", Utilization: 0.08, ResetAt: routerTestNow.Add(-time.Hour)})
	setMeter(state, seat.ID, "claude", Meter{Name: "7d_fable", Utilization: 0.4, ResetAt: routerTestNow.Add(48 * time.Hour)})

	view := state.Dashboard([]*Auth{seat}, routerTestNow)
	meters := view.Accounts[seat.ID].Meters
	if len(meters) != 3 || meters[0].Name != "5h" || meters[1].Name != "7d" || meters[2].Title != "Fable week" {
		t.Fatalf("meters = %+v", meters)
	}
	if !meters[1].ResetSinceReading || meters[1].Utilization == nil || *meters[1].Utilization != 0 {
		t.Fatalf("passed weekly reset should show a fresh week: %+v", meters[1])
	}
	if view.Mode != "shadow" || len(view.Rankings) != 1 || view.Rankings[0].Candidates[0].AuthID != seat.ID || view.Shadow == nil {
		t.Fatalf("view = %+v", view)
	}
}

func TestUpstreamErrorCode(t *testing.T) {
	cases := map[string]*Error{
		"oauth_not_allowed_for_organization": {Message: `{"type":"error","error":{"type":"permission_error","details":{"error_code":"oauth_not_allowed_for_organization"}}}`},
		"permission_error":                   {Message: `{"type":"error","error":{"type":"permission_error","message":"no"}}`},
		"plain_code":                         {Code: "plain_code", Message: "not json"},
	}
	for want, err := range cases {
		if got := upstreamErrorCode(err); got != want {
			t.Fatalf("upstreamErrorCode(%q) = %q, want %q", err.Message, got, want)
		}
	}
}

// A reset or status header without a used share must not read as 0% used:
// it creates no known reading, keeps an earlier one, and old saved states,
// which have no unknown flag, load their utilization as known.
func TestMeterWithoutUtilizationStaysUnknown(t *testing.T) {
	state := newTestRoutingState(routerTestNow)
	auth := routerTestAuth("auth-u", "claude", "u@example.com")
	resetOnly := http.Header{}
	resetOnly.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(routerTestNow.Add(48*time.Hour).Unix(), 10))
	resetOnly.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	state.observeResult(Result{AuthID: auth.ID, Provider: "claude", Success: true}, resetOnly, routerTestNow)

	week := state.account(auth.ID).Meters["7d"]
	if week == nil || !week.UtilizationUnknown || !week.ObservedAt.IsZero() || len(state.account(auth.ID).History["7d"]) != 0 {
		t.Fatalf("reset-only meter = %+v, want unknown with no reading time or history", week)
	}
	if view := state.Dashboard([]*Auth{auth}, routerTestNow).Accounts[auth.ID].Meters[0]; view.Utilization != nil {
		t.Fatalf("dashboard utilization = %v, want null", *view.Utilization)
	}
	if !state.NeedsMeterReading(auth.ID, routerTestNow, time.Minute) {
		t.Fatal("an unknown meter should still be probed")
	}
	if c := NewScoredSelector(ScorerConfig{}, state).assess(auth, "claude-opus-5-5", 0, routerTestNow); c.Excluded || len(c.Problems) == 0 || c.Problems[0] != "no weekly reading yet" {
		t.Fatalf("candidate = %+v, want no weekly reading, not used up or empty", c)
	}

	known := http.Header{}
	known.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.3")
	state.observeResult(Result{AuthID: auth.ID, Provider: "claude", Success: true}, known, routerTestNow)
	later := routerTestNow.Add(10 * time.Minute)
	resetOnly.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(routerTestNow.Add(72*time.Hour).Unix(), 10))
	state.observeResult(Result{AuthID: auth.ID, Provider: "claude", Success: true}, resetOnly, later)
	week = state.account(auth.ID).Meters["7d"]
	if week.UtilizationUnknown || week.Utilization != 0.3 || !week.ObservedAt.Equal(routerTestNow) || !week.ResetAt.Equal(routerTestNow.Add(72*time.Hour)) {
		t.Fatalf("merged meter = %+v, want 0.3 read at the first time with the new reset", week)
	}

	var old Meter
	if errUnmarshal := json.Unmarshal([]byte(`{"name":"7d","utilization":0.4,"observed_at":"2026-10-04T15:00:00Z"}`), &old); errUnmarshal != nil || old.UtilizationUnknown || old.Utilization != 0.4 {
		t.Fatalf("old saved meter = %+v (%v), want known 0.4", old, errUnmarshal)
	}
	raw, _ := json.Marshal(Meter{Name: "5h", UtilizationUnknown: true})
	var back Meter
	if errUnmarshal := json.Unmarshal(raw, &back); errUnmarshal != nil || !back.UtilizationUnknown {
		t.Fatalf("round trip of %s = %+v, want unknown kept", raw, back)
	}
}

// A reset-only header for the next window must not carry the ended window's
// share onto it: the new window starts at a known zero with no old status.
func TestUnknownReadingAfterResetStartsTheNewWindowAtZero(t *testing.T) {
	state := newTestRoutingState(routerTestNow)
	reset := routerTestNow.Add(time.Hour)
	next := reset.Add(weeklyQuotaWindow)
	observe := func(auth *Auth, at time.Time, fields map[string]string) {
		headers := http.Header{}
		for key, value := range fields {
			headers.Set("Anthropic-Ratelimit-Unified-7d-"+key, value)
		}
		state.observeResult(Result{AuthID: auth.ID, Provider: "claude", Success: true}, headers, at)
	}
	unix := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }

	// 99% used, the reset passes, then a header names only the next reset.
	full := routerTestAuth("auth-full", "claude", "full@example.com")
	observe(full, routerTestNow, map[string]string{"Utilization": "0.99", "Reset": unix(reset), "Status": "allowed"})
	later := routerTestNow.Add(2 * time.Hour)
	observe(full, later, map[string]string{"Reset": unix(next), "Status": "allowed"})
	week := state.account(full.ID).Meters["7d"]
	if week.UtilizationUnknown || week.Utilization != 0 || !week.ResetAt.Equal(next) || !week.ObservedAt.Equal(reset) || week.Status != "allowed" {
		t.Fatalf("meter = %+v, want a known 0 read at the reset, with the next reset", week)
	}
	if view := state.Dashboard([]*Auth{full}, later).Accounts[full.ID].Meters[0]; view.Utilization == nil || *view.Utilization != 0 || view.ResetSinceReading {
		t.Fatalf("dashboard meter = %+v, want a fresh 0", view)
	}
	if c := NewScoredSelector(ScorerConfig{}, state).assess(full, "claude-opus-5-5", 0, later); c.Excluded || !strings.Contains(c.Summary, "100% of") {
		t.Fatalf("candidate = %+v, want 100%% of the week left", c)
	}

	// Rejected, and the upstream has moved to the next window before the
	// stored reset: the old verdict must not keep the account out.
	rejected := routerTestAuth("auth-rejected", "claude", "rejected@example.com")
	observe(rejected, routerTestNow, map[string]string{"Utilization": "0.99", "Reset": unix(reset), "Status": "rejected"})
	soon := routerTestNow.Add(30 * time.Minute)
	observe(rejected, soon, map[string]string{"Reset": unix(next)})
	if week = state.account(rejected.ID).Meters["7d"]; week.Status != "" || week.Utilization != 0 || !week.ObservedAt.Equal(soon) {
		t.Fatalf("meter = %+v, want the old verdict and share dropped", week)
	}
	if c := NewScoredSelector(ScorerConfig{}, state).assess(rejected, "claude-opus-5-5", 0, soon); c.Excluded {
		t.Fatalf("candidate = %+v, want it back in rotation", c)
	}
}
