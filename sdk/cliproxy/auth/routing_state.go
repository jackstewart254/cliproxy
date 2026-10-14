package auth

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	routingStateFileVersion = 1
	routingStateFlushEvery  = time.Minute
	// meterSampleSpacing is the minimum gap between stored history samples of
	// one meter, unless its utilization moved.
	meterSampleSpacing = 2 * time.Minute
	maxMeterSamples    = 4000
	maxMeterHistory    = 8 * 24 * time.Hour
	// refusalQuarantine is how long a credential the provider refused stays out
	// of new placements, unless it answers successfully in the meantime.
	refusalQuarantine = 6 * time.Hour
	// activeSessionWindow is how recently a session must have sent a request to
	// count as active on its credential.
	activeSessionWindow = 15 * time.Minute
	maxShadowDecisions  = 2000
	maxShadowAge        = 7 * 24 * time.Hour
)

// AccountHealth tracks whether the provider still accepts a credential.
type AccountHealth struct {
	LastSuccessAt     time.Time `json:"last_success_at,omitempty"`
	LastFailureAt     time.Time `json:"last_failure_at,omitempty"`
	LastFailureStatus int       `json:"last_failure_status,omitempty"`
	LastFailureCode   string    `json:"last_failure_code,omitempty"`
}

// accountRouting is everything the router remembers about one credential.
type accountRouting struct {
	Provider string                   `json:"provider"`
	Meters   map[string]*Meter        `json:"meters,omitempty"`
	History  map[string][]MeterSample `json:"history,omitempty"`
	Facts    AccountFacts             `json:"facts"`
	Health   AccountHealth            `json:"health"`
}

func (a *accountRouting) clone() *accountRouting {
	if a == nil {
		return nil
	}
	copyAccount := &accountRouting{Provider: a.Provider, Facts: a.Facts, Health: a.Health}
	if len(a.Meters) > 0 {
		copyAccount.Meters = make(map[string]*Meter, len(a.Meters))
		for name, meter := range a.Meters {
			copyMeter := *meter
			copyMeter.Models = append([]string(nil), meter.Models...)
			copyAccount.Meters[name] = &copyMeter
		}
	}
	if len(a.History) > 0 {
		copyAccount.History = make(map[string][]MeterSample, len(a.History))
		for name, samples := range a.History {
			copyAccount.History[name] = append([]MeterSample(nil), samples...)
		}
	}
	return copyAccount
}

// ShadowDecision records one new-session placement: what the live selector
// chose and what the scored selector would have chosen.
type ShadowDecision struct {
	At       time.Time `json:"at"`
	Provider string    `json:"provider"`
	Model    string    `json:"model,omitempty"`
	Session  string    `json:"session,omitempty"`
	Live     string    `json:"live"`
	Shadow   string    `json:"shadow"`
	// Ranking is kept on disagreements so the dashboard can say why.
	Ranking []RouteCandidate `json:"ranking,omitempty"`
}

type routingStateFile struct {
	Version   int                        `json:"version"`
	Accounts  map[string]*accountRouting `json:"accounts"`
	Decisions []ShadowDecision           `json:"decisions,omitempty"`
	Bindings  []SessionBinding           `json:"bindings,omitempty"`
}

// RoutingState is the router's memory: every meter each credential reported,
// their history, credential health, shadow decisions and session bindings. It
// survives restarts through a JSON file beside the config.
type RoutingState struct {
	mu        sync.Mutex
	path      string
	accounts  map[string]*accountRouting
	decisions []ShadowDecision
	// cache is the live session-affinity cache, when one is attached.
	cache *SessionCache
	// pendingBindings are bindings waiting for a cache to be attached.
	pendingBindings []SessionBinding
	scorerMode      string
	scorerConfig    ScorerConfig
	dirty           bool
	nowFunc         func() time.Time
	loopOnce        sync.Once
}

var defaultRoutingState = NewRoutingState()

// NewRoutingState returns an empty state that is not persisted.
func NewRoutingState() *RoutingState {
	return &RoutingState{accounts: make(map[string]*accountRouting), nowFunc: time.Now}
}

// DefaultRoutingState returns the process-wide routing state.
func DefaultRoutingState() *RoutingState { return defaultRoutingState }

// ConfigureRoutingState sets the persistence file of the process-wide state,
// loads what it holds and starts the periodic flush.
func ConfigureRoutingState(path string) {
	defaultRoutingState.Configure(path)
}

// Configure sets the persistence file, loads it and starts the periodic
// flush. Calling it again with the same path does nothing.
func (s *RoutingState) Configure(path string) {
	path = strings.TrimSpace(path)
	if s == nil || path == "" {
		return
	}
	s.mu.Lock()
	if s.path == path {
		s.mu.Unlock()
		return
	}
	s.path = path
	s.loadLocked()
	s.mu.Unlock()
	s.loopOnce.Do(func() { go s.flushLoop() })
}

func (s *RoutingState) flushLoop() {
	ticker := time.NewTicker(routingStateFlushEvery)
	defer ticker.Stop()
	for range ticker.C {
		if errFlush := s.Flush(); errFlush != nil {
			log.Warnf("routing state: flush failed: %v", errFlush)
		}
	}
}

func (s *RoutingState) now() time.Time {
	if s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

// Flush writes the state to disk. Session bindings change on every request,
// so an attached cache always triggers a write.
func (s *RoutingState) Flush() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.path == "" || (!s.dirty && s.cache == nil) {
		s.mu.Unlock()
		return nil
	}
	now := s.now()
	s.pruneLocked(now)
	state := routingStateFile{
		Version:   routingStateFileVersion,
		Accounts:  s.accounts,
		Decisions: s.decisions,
		Bindings:  s.bindingsLocked(),
	}
	data, errMarshal := json.Marshal(state)
	path := s.path
	s.dirty = false
	s.mu.Unlock()
	if errMarshal != nil {
		return errMarshal
	}
	tmp := path + ".tmp"
	if errWrite := os.WriteFile(tmp, data, 0o600); errWrite != nil {
		return errWrite
	}
	return os.Rename(tmp, path)
}

func (s *RoutingState) loadLocked() {
	data, errRead := os.ReadFile(s.path)
	if errRead != nil {
		if !os.IsNotExist(errRead) {
			log.Warnf("routing state: read %s: %v", filepath.Base(s.path), errRead)
		}
		return
	}
	var state routingStateFile
	if errUnmarshal := json.Unmarshal(data, &state); errUnmarshal != nil {
		log.Warnf("routing state: parse %s: %v", filepath.Base(s.path), errUnmarshal)
		return
	}
	for authID, account := range state.Accounts {
		if account == nil || s.accounts[authID] != nil {
			continue
		}
		if account.Meters == nil {
			account.Meters = make(map[string]*Meter)
		}
		if account.History == nil {
			account.History = make(map[string][]MeterSample)
		}
		s.accounts[authID] = account
	}
	s.decisions = append(state.Decisions, s.decisions...)
	if s.cache != nil {
		s.cache.Restore(state.Bindings)
	} else {
		s.pendingBindings = state.Bindings
	}
}

func (s *RoutingState) bindingsLocked() []SessionBinding {
	if s.cache != nil {
		return s.cache.Snapshot()
	}
	return s.pendingBindings
}

func (s *RoutingState) pruneLocked(now time.Time) {
	for _, account := range s.accounts {
		for name, samples := range account.History {
			cut := 0
			for cut < len(samples) && now.Sub(samples[cut].At) > maxMeterHistory {
				cut++
			}
			if cut > 0 {
				account.History[name] = append([]MeterSample(nil), samples[cut:]...)
			}
		}
	}
	cut := 0
	for cut < len(s.decisions) && now.Sub(s.decisions[cut].At) > maxShadowAge {
		cut++
	}
	if extra := len(s.decisions) - cut - maxShadowDecisions; extra > 0 {
		cut += extra
	}
	if cut > 0 {
		s.decisions = append([]ShadowDecision(nil), s.decisions[cut:]...)
	}
}

// AttachSessionCache hands the live session-affinity cache to the state. The
// bindings of the previous cache, or of the last run, are restored into it, so
// a restart or a routing change does not move running sessions.
func (s *RoutingState) AttachSessionCache(cache *SessionCache) {
	if s == nil || cache == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == cache {
		return
	}
	bindings := s.bindingsLocked()
	cache.Restore(bindings)
	s.cache = cache
	s.pendingBindings = nil
}

// SetScorer records the scored selector's mode and settings for the dashboard.
func (s *RoutingState) SetScorer(mode string, cfg ScorerConfig) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.scorerMode = mode
	s.scorerConfig = cfg
	s.mu.Unlock()
}

// Scorer returns the scored selector's mode and settings.
func (s *RoutingState) Scorer() (string, ScorerConfig) {
	if s == nil {
		return "", ScorerConfig{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scorerMode, s.scorerConfig
}

// observeResult records what one upstream result says about its credential:
// its meters (unless the result skips quota observation) and its health.
func (s *RoutingState) observeResult(result Result, headers http.Header, now time.Time) {
	if s == nil || strings.TrimSpace(result.AuthID) == "" {
		return
	}
	model := strings.TrimSpace(result.Model)
	if model == "" {
		model = strings.TrimSpace(result.RouteModel)
	}
	var meters map[string]*Meter
	var facts AccountFacts
	if !result.SkipQuotaObservation {
		meters, facts = parseMeters(result.Provider, headers, now)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	account := s.accounts[result.AuthID]
	if account == nil {
		account = &accountRouting{Meters: make(map[string]*Meter), History: make(map[string][]MeterSample)}
		s.accounts[result.AuthID] = account
	}
	if provider := strings.TrimSpace(result.Provider); provider != "" {
		account.Provider = strings.ToLower(provider)
	}
	if result.Success {
		account.Health.LastSuccessAt = now
	} else if result.Error != nil && result.Error.StatusCode() > 0 {
		account.Health.LastFailureAt = now
		account.Health.LastFailureStatus = result.Error.StatusCode()
		account.Health.LastFailureCode = upstreamErrorCode(result.Error)
	}
	mergeMeters(account, meters, model, now)
	if len(meters) > 0 {
		facts.LastModel = model
		facts.ObservedAt = now
		account.Facts = facts
	}
	s.dirty = true
}

// mergeMeters folds observed meters into an account and its history. An
// observation without a used share keeps the last known one, and its reading
// time, and only refreshes the reset, window and any status it carries.
func mergeMeters(account *accountRouting, meters map[string]*Meter, model string, now time.Time) {
	for name, observed := range meters {
		existing := account.Meters[name]
		if existing == nil {
			existing = &Meter{Name: name, UtilizationUnknown: true}
			account.Meters[name] = existing
		}
		if !observed.ResetAt.IsZero() {
			existing.ResetAt = observed.ResetAt
		}
		if observed.Window > 0 {
			existing.Window = observed.Window
		}
		if observed.Label != "" {
			existing.Label = observed.Label
		}
		existing.Models = addMeterModel(existing.Models, model)
		if observed.UtilizationUnknown {
			if observed.Status != "" {
				existing.Status = observed.Status
			}
			continue
		}
		existing.Utilization, existing.UtilizationUnknown = observed.Utilization, false
		existing.Status = observed.Status
		existing.ObservedAt = now
		account.History[name] = appendMeterSample(account.History[name], MeterSample{At: now, Utilization: observed.Utilization})
	}
}

func appendMeterSample(samples []MeterSample, sample MeterSample) []MeterSample {
	if n := len(samples); n > 0 {
		last := samples[n-1]
		if sample.At.Sub(last.At) < meterSampleSpacing && sample.Utilization == last.Utilization {
			return samples
		}
	}
	samples = append(samples, sample)
	if len(samples) > maxMeterSamples {
		samples = append([]MeterSample(nil), samples[len(samples)-maxMeterSamples:]...)
	}
	return samples
}

// upstreamErrorCode pulls a short machine-readable code out of an upstream
// error body, such as "oauth_not_allowed_for_organization".
func upstreamErrorCode(err *Error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Message)
	if gjson.Valid(message) {
		for _, path := range []string{"error.details.error_code", "error.code", "error.type", "detail.code", "code", "type"} {
			if value := strings.TrimSpace(gjson.Get(message, path).String()); value != "" && value != "error" {
				return truncateString(value, 64)
			}
		}
	}
	return truncateString(strings.TrimSpace(err.Code), 64)
}

// refusedReason reports whether the provider recently refused the credential
// outright (402, or 403 with a permission error) and has not accepted it since.
func (h AccountHealth) refusedReason(now time.Time) (string, bool) {
	if h.LastFailureAt.IsZero() || !h.LastFailureAt.After(h.LastSuccessAt) || now.Sub(h.LastFailureAt) >= refusalQuarantine {
		return "", false
	}
	switch h.LastFailureStatus {
	case http.StatusPaymentRequired:
	case http.StatusForbidden:
		code := strings.ToLower(h.LastFailureCode)
		if !strings.Contains(code, "permission") && !strings.Contains(code, "not_allowed") && !strings.Contains(code, "forbidden") {
			return "", false
		}
	default:
		return "", false
	}
	reason := "refused by the provider"
	if h.LastFailureCode != "" {
		reason += " (" + h.LastFailureCode + ")"
	}
	return reason, true
}

// RefusedReason reports whether the provider recently refused the credential.
func (s *RoutingState) RefusedReason(authID string, now time.Time) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	account := s.accounts[authID]
	if account == nil {
		return "", false
	}
	return account.Health.refusedReason(now)
}

// withoutRefused drops credentials the provider recently refused. When every
// candidate is refused the list is returned unchanged, so routing never fails
// closed on stale health data.
func (s *RoutingState) withoutRefused(auths []*Auth, now time.Time) []*Auth {
	if s == nil || len(auths) < 2 {
		return auths
	}
	kept := make([]*Auth, 0, len(auths))
	for _, candidate := range auths {
		if candidate == nil {
			continue
		}
		if _, refused := s.RefusedReason(candidate.ID, now); !refused {
			kept = append(kept, candidate)
		}
	}
	if len(kept) == 0 {
		return auths
	}
	return kept
}

// account returns a copy of what the state holds for one credential.
func (s *RoutingState) account(authID string) *accountRouting {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accounts[authID].clone()
}

// activeSessions counts, per credential, the sessions that sent a request in
// the last activeSessionWindow.
func (s *RoutingState) activeSessions(now time.Time) map[string]int {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	cache := s.cache
	s.mu.Unlock()
	if cache == nil {
		return nil
	}
	return cache.ActiveSessionsByAuth(now.Add(-activeSessionWindow))
}

// meterResetDrop is how far a reading must fall below the highest reading
// since the last reset to count as a reset. Responses finish out of order, so
// a reading can arrive a point below one taken moments earlier; that is not a
// reset.
const meterResetDrop = 0.03

// longBurnLookback is the burn window for weekly-class meters. They report
// whole points and move about half a point an hour, so 90 minutes of samples
// often shows no rise at all.
const longBurnLookback = 6 * time.Hour

// meterBurnLookback returns how far back burnRate should look for a meter.
func meterBurnLookback(meter *Meter) time.Duration {
	if meterIsLong(meter) {
		return longBurnLookback
	}
	return burnLookback
}

// burnRate estimates how fast a meter is filling, as utilization per hour,
// from samples taken since the last reset within the lookback period. The rise
// is measured to the highest reading, so a late, lower reply does not shrink
// it. It reports false when there is too little history.
func burnRate(samples []MeterSample, now time.Time, lookback time.Duration) (float64, bool) {
	start := -1
	high := 0.0
	for i, sample := range samples {
		if now.Sub(sample.At) > lookback {
			continue
		}
		if start < 0 || sample.Utilization < high-meterResetDrop {
			start, high = i, sample.Utilization
			continue
		}
		if sample.Utilization > high {
			high = sample.Utilization
		}
	}
	if start < 0 || start == len(samples)-1 {
		return 0, false
	}
	elapsed := samples[len(samples)-1].At.Sub(samples[start].At)
	if elapsed < 10*time.Minute {
		return 0, false
	}
	return (high - samples[start].Utilization) / elapsed.Hours(), true
}

// recordDecision stores one shadow comparison.
func (s *RoutingState) recordDecision(decision ShadowDecision) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if decision.Live == decision.Shadow {
		decision.Ranking = nil
	}
	s.decisions = append(s.decisions, decision)
	if len(s.decisions) > maxShadowDecisions+100 {
		s.pruneLocked(decision.At)
	}
	s.dirty = true
}

// ShadowSummary is the dashboard view of the shadow comparison.
type ShadowSummary struct {
	Since        time.Time         `json:"since"`
	Choices      int               `json:"choices"`
	Agreed       int               `json:"agreed"`
	ByProvider   map[string][2]int `json:"by_provider"`
	Disagreed    []ShadowDecision  `json:"disagreed"`
	LastDecision *ShadowDecision   `json:"last_decision,omitempty"`
}

// shadowSummary counts the decisions of the last 24 hours and returns the
// most recent disagreements.
func (s *RoutingState) shadowSummary(now time.Time, limit int) ShadowSummary {
	summary := ShadowSummary{Since: now.Add(-24 * time.Hour), ByProvider: make(map[string][2]int)}
	if s == nil {
		return summary
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.decisions) - 1; i >= 0; i-- {
		decision := s.decisions[i]
		if summary.LastDecision == nil {
			copyDecision := decision
			summary.LastDecision = &copyDecision
		}
		if decision.At.Before(summary.Since) {
			break
		}
		counts := summary.ByProvider[decision.Provider]
		counts[0]++
		summary.Choices++
		if decision.Live == decision.Shadow {
			counts[1]++
			summary.Agreed++
		} else if len(summary.Disagreed) < limit {
			summary.Disagreed = append(summary.Disagreed, decision)
		}
		summary.ByProvider[decision.Provider] = counts
	}
	return summary
}

// lastModelFor returns the model of the most recent response the provider
// served, used to rank candidates for the dashboard.
func (s *RoutingState) lastModelFor(provider string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var model string
	var at time.Time
	for _, account := range s.accounts {
		if account.Provider == provider && account.Facts.ObservedAt.After(at) && account.Facts.LastModel != "" {
			model, at = account.Facts.LastModel, account.Facts.ObservedAt
		}
	}
	return model
}

// accountIDs returns the credentials the state knows, sorted.
func (s *RoutingState) accountIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.accounts))
	for id := range s.accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
