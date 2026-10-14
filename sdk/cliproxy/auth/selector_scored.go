package auth

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	defaultMaxSessionsPerAccount = 6
	// fullMeterUtilization is the share at which a window counts as used up.
	fullMeterUtilization = 0.98
	// shortWindowCaution is the 5-hour share above which a new session risks
	// having to move before the window resets.
	shortWindowCaution = 0.85
	// shortWindowProjectedLimit is the projected 5-hour share at reset above
	// which the window is on course to run out.
	shortWindowProjectedLimit = 0.95
	burnLookback              = 90 * time.Minute
	staleReadingAge           = 3 * time.Hour
	// unknownWeekHours is the window assumed for a credential with no weekly
	// reading: a fresh week.
	unknownWeekHours = 7 * 24.0
	minPressureHours = 0.25
)

// ScorerConfig holds the scored selector's settings.
type ScorerConfig struct {
	// Headroom maps a lower-case account email or auth ID to the highest
	// 5-hour utilization (0 to 1) the pool should push that account to while
	// another account has room. It protects a seat its owner also uses directly.
	Headroom map[string]float64 `json:"headroom,omitempty"`
	// MaxSessions is how many active sessions make an account count as busy.
	// Zero means defaultMaxSessionsPerAccount.
	MaxSessions int `json:"max_sessions,omitempty"`
}

func (c ScorerConfig) maxSessions() int {
	if c.MaxSessions > 0 {
		return c.MaxSessions
	}
	return defaultMaxSessionsPerAccount
}

// RouteCandidate is one credential's standing for a new session.
type RouteCandidate struct {
	AuthID  string `json:"auth_id"`
	Account string `json:"account"`
	// Excluded marks a credential that cannot take the session at all.
	Excluded bool `json:"excluded,omitempty"`
	// Problems are soft issues; each one ranks the credential lower.
	Problems []string `json:"problems,omitempty"`
	// Pressure is how fast, in percentage points per hour, the credential must
	// be used to spend its binding long window before it resets.
	Pressure float64 `json:"pressure"`
	// Summary explains the standing in one line.
	Summary string `json:"summary"`
}

// ScoredSelector places new sessions by least laxity: the credential whose
// remaining long-window allowance is most at risk of expiring unused goes
// first, unless a soft problem ranks it lower.
//
// For each credential it reads every meter the requested model draws on from
// the routing state. Hard filters exclude a credential the provider refused or
// whose window is full. The main score is the allowance left on the binding
// long window divided by the hours until that window resets. Soft problems
// (5-hour window on course to run out, above its owner's headroom line, busy,
// stale or missing reading) each drop it one tier. Credentials sort by tier,
// then by score.
type ScoredSelector struct {
	cfg     ScorerConfig
	state   *RoutingState
	nowFunc func() time.Time
}

// NewScoredSelector builds the selector. A nil state uses the process-wide one.
func NewScoredSelector(cfg ScorerConfig, state *RoutingState) *ScoredSelector {
	if state == nil {
		state = defaultRoutingState
	}
	normalized := ScorerConfig{MaxSessions: cfg.MaxSessions}
	for key, line := range cfg.Headroom {
		if key = strings.ToLower(strings.TrimSpace(key)); key != "" && line > 0 {
			if normalized.Headroom == nil {
				normalized.Headroom = make(map[string]float64)
			}
			normalized.Headroom[key] = line
		}
	}
	return &ScoredSelector{cfg: normalized, state: state}
}

func (s *ScoredSelector) now() time.Time {
	if s != nil && s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

// Pick selects the best-ranked credential.
func (s *ScoredSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := s.now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if len(available) == 1 {
		return available[0], nil
	}
	return bestCandidate(available, s.rankAvailable(model, available, now)), nil
}

// bestCandidate returns the first ranked credential that is not excluded, or
// the first available one when every credential is excluded, so routing never
// fails closed on the router's own data.
func bestCandidate(available []*Auth, ranking []RouteCandidate) *Auth {
	for _, candidate := range ranking {
		if candidate.Excluded {
			continue
		}
		for _, auth := range available {
			if auth.ID == candidate.AuthID {
				return auth
			}
		}
	}
	return available[0]
}

// rankAvailable ranks credentials that already passed the availability and
// priority checks.
func (s *ScoredSelector) rankAvailable(model string, available []*Auth, now time.Time) []RouteCandidate {
	sessions := s.state.activeSessions(now)
	ranking := make([]RouteCandidate, 0, len(available))
	for _, auth := range available {
		if auth == nil {
			continue
		}
		ranking = append(ranking, s.assess(auth, model, sessions[auth.ID], now))
	}
	sortCandidates(ranking)
	return ranking
}

func sortCandidates(ranking []RouteCandidate) {
	sort.SliceStable(ranking, func(i, j int) bool {
		left, right := ranking[i], ranking[j]
		if left.Excluded != right.Excluded {
			return !left.Excluded
		}
		if len(left.Problems) != len(right.Problems) {
			return len(left.Problems) < len(right.Problems)
		}
		if left.Pressure != right.Pressure {
			return left.Pressure > right.Pressure
		}
		return left.AuthID < right.AuthID
	})
}

// Rank explains how every credential would stand for a new session of model,
// including the ones that are off, cooling down or held in reserve.
func (s *ScoredSelector) Rank(model string, auths []*Auth, now time.Time) []RouteCandidate {
	byPriority, _, _ := collectAvailableByPriority(auths, model, now)
	available := availableAuthsFromPriorityBuckets(byPriority, false)
	ranking := s.rankAvailable(model, available, now)
	eligible := make(map[string]bool, len(available))
	for _, auth := range available {
		eligible[auth.ID] = true
	}
	excluded := make([]RouteCandidate, 0)
	for _, auth := range auths {
		if auth == nil || eligible[auth.ID] {
			continue
		}
		candidate := RouteCandidate{AuthID: auth.ID, Account: authAccountLabel(auth), Excluded: true}
		blocked, reason, next := isAuthBlockedForModel(auth, model, now)
		switch {
		case blocked && reason == blockReasonDisabled:
			candidate.Summary = "Off"
		case blocked && !next.IsZero():
			candidate.Summary = "Cooling down until " + formatRouteTime(next, now)
		case blocked:
			candidate.Summary = "Unavailable"
		default:
			candidate.Summary = "Reserve: used when every rotation account is out"
		}
		excluded = append(excluded, candidate)
	}
	sort.SliceStable(excluded, func(i, j int) bool { return excluded[i].AuthID < excluded[j].AuthID })
	return append(ranking, excluded...)
}

// assess scores one available credential.
func (s *ScoredSelector) assess(auth *Auth, model string, activeSessions int, now time.Time) RouteCandidate {
	candidate := RouteCandidate{AuthID: auth.ID, Account: authAccountLabel(auth)}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	account := s.state.account(auth.ID)
	if account == nil {
		account = &accountRouting{}
	}
	if reason, refused := account.Health.refusedReason(now); refused {
		candidate.Excluded = true
		candidate.Summary = capitalize(reason)
		return candidate
	}

	var long, short []Meter
	var newest time.Time
	current := false
	for _, meter := range account.Meters {
		if !meterAppliesToModel(provider, meter, model) {
			continue
		}
		effective, wasReset := effectiveMeter(*meter, now)
		if !wasReset {
			current = true
			if meter.ObservedAt.After(newest) {
				newest = meter.ObservedAt
			}
			if effective.ResetAt.After(now) && (strings.EqualFold(effective.Status, "rejected") || effective.Utilization >= fullMeterUtilization) {
				candidate.Excluded = true
				candidate.Summary = capitalize(meterTitle(provider, &effective)) + " used up until " + formatRouteTime(effective.ResetAt, now)
				return candidate
			}
		}
		// An unknown used share is neither empty nor full: leave it out, as if
		// the meter had no reading.
		if effective.UtilizationUnknown {
			continue
		}
		switch {
		case meterIsLong(&effective):
			long = append(long, effective)
		case meterIsShort(&effective):
			short = append(short, effective)
		}
	}

	parts := make([]string, 0, 3)
	if len(long) == 0 {
		candidate.Pressure = 100 / unknownWeekHours
		candidate.Problems = append(candidate.Problems, "no weekly reading yet")
	} else {
		binding := long[0]
		for _, meter := range long[1:] {
			if meter.Utilization > binding.Utilization || (meter.Utilization == binding.Utilization && meter.Name < binding.Name) {
				binding = meter
			}
		}
		left := math.Max(0, 1-binding.Utilization)
		hours := unknownWeekHours
		if !binding.ResetAt.IsZero() {
			hours = math.Max(minPressureHours, binding.ResetAt.Sub(now).Hours())
		}
		candidate.Pressure = left * 100 / hours
		resetText := "unknown reset"
		if !binding.ResetAt.IsZero() {
			resetText = "resets " + formatRouteTime(binding.ResetAt, now)
		}
		parts = append(parts, fmt.Sprintf("%s of %s left, %s, so it needs %s an hour",
			percentText(left), meterTitle(provider, &binding), resetText, pressureText(candidate.Pressure)))
	}

	if len(short) > 0 {
		window := short[0]
		for _, meter := range short[1:] {
			if meter.Utilization > window.Utilization {
				window = meter
			}
		}
		text := fmt.Sprintf("5-hour window at %s", percentText(window.Utilization))
		rate, known := burnRate(account.History[window.Name], now, burnLookback)
		if known && rate > 0 {
			text += fmt.Sprintf(", rising %s an hour", percentText(rate))
		}
		parts = append(parts, text)
		if line := s.headroomFor(auth); line > 0 && window.Utilization >= line {
			candidate.Problems = append(candidate.Problems, fmt.Sprintf("above the %s line kept free for its owner", percentText(line)))
		}
		projected := window.Utilization
		if known && rate > 0 && window.ResetAt.After(now) {
			projected += rate * window.ResetAt.Sub(now).Hours()
		}
		if window.Utilization >= shortWindowCaution || projected >= shortWindowProjectedLimit {
			until := ""
			if !window.ResetAt.IsZero() {
				until = " before " + formatRouteTime(window.ResetAt, now)
			}
			candidate.Problems = append(candidate.Problems, "5-hour window on course to run out"+until)
		}
	}

	if activeSessions > 0 {
		parts = append(parts, plural(activeSessions, "active session", "active sessions"))
	}
	if limit := s.cfg.maxSessions(); activeSessions >= limit {
		candidate.Problems = append(candidate.Problems, fmt.Sprintf("already serving %d sessions", activeSessions))
	}
	if current && !newest.IsZero() && now.Sub(newest) > staleReadingAge {
		candidate.Problems = append(candidate.Problems, "reading "+ageText(now.Sub(newest))+" old")
	}
	candidate.Summary = capitalize(strings.Join(parts, " · "))
	return candidate
}

func (s *ScoredSelector) headroomFor(auth *Auth) float64 {
	if s == nil || len(s.cfg.Headroom) == 0 || auth == nil {
		return 0
	}
	for _, key := range []string{authAccountLabel(auth), auth.ID, auth.Label} {
		if line, ok := s.cfg.Headroom[strings.ToLower(strings.TrimSpace(key))]; ok {
			return line
		}
	}
	return 0
}

// authAccountLabel names a credential the way the dashboard does: its email,
// else its label, else its ID.
func authAccountLabel(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if email, ok := auth.Metadata["email"].(string); ok && strings.TrimSpace(email) != "" {
		return strings.TrimSpace(email)
	}
	if label := strings.TrimSpace(auth.Label); label != "" {
		return label
	}
	return auth.ID
}

// meterTitle names a meter in plain words: "week", "5-hour window",
// "Fable week".
func meterTitle(provider string, meter *Meter) string {
	if meter == nil {
		return ""
	}
	base := "window"
	switch {
	case meterIsLong(meter):
		base = "week"
	case meterIsShort(meter):
		base = "5-hour window"
	}
	if meter.Label != "" {
		return meter.Label + " " + base
	}
	if strings.EqualFold(provider, "claude") {
		if family := claudeModelFamily(meter.Name); family != "" {
			return capitalize(family) + " " + base
		}
	}
	if strings.EqualFold(provider, "codex") && meter.Name != "primary" && meter.Name != "secondary" {
		return strings.TrimSuffix(strings.TrimSuffix(meter.Name, "-primary"), "-secondary") + " " + base
	}
	return base
}

func formatRouteTime(t, now time.Time) string {
	local := t.In(time.Local)
	if local.Sub(now) < 20*time.Hour && local.YearDay() == now.In(time.Local).YearDay() {
		return local.Format("15:04")
	}
	return local.Format("Mon 15:04")
}

func percentText(share float64) string {
	value := share * 100
	if value > 0 && value < 1 {
		return fmt.Sprintf("%.1f%%", value)
	}
	return fmt.Sprintf("%.0f%%", value)
}

func pressureText(pointsPerHour float64) string {
	if pointsPerHour < 10 {
		return fmt.Sprintf("%.1f%%", pointsPerHour)
	}
	return fmt.Sprintf("%.0f%%", pointsPerHour)
}

func ageText(age time.Duration) string {
	if age < time.Hour {
		return fmt.Sprintf("%dm", int(age.Minutes()))
	}
	if age < 48*time.Hour {
		return fmt.Sprintf("%dh", int(age.Hours()))
	}
	return fmt.Sprintf("%dd", int(age.Hours()/24))
}

func plural(count int, one, many string) string {
	if count == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", count, many)
}

func capitalize(text string) string {
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// ShadowSelector routes with the live selector and records, for every
// new-session placement with a real choice, what the scored selector would
// have picked. It never changes the live choice.
type ShadowSelector struct {
	live   Selector
	scorer *ScoredSelector
	state  *RoutingState
}

// NewShadowSelector wraps live with a shadow comparison.
func NewShadowSelector(live Selector, scorer *ScoredSelector, state *RoutingState) *ShadowSelector {
	if state == nil {
		state = defaultRoutingState
	}
	return &ShadowSelector{live: live, scorer: scorer, state: state}
}

// Pick returns the live selector's choice.
func (s *ShadowSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	picked, err := s.live.Pick(ctx, provider, model, opts, auths)
	if err != nil || picked == nil || s.scorer == nil {
		return picked, err
	}
	now := s.scorer.now()
	available, errAvailable := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if errAvailable != nil {
		return picked, nil
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if len(available) < 2 {
		return picked, nil
	}
	ranking := s.scorer.rankAvailable(model, available, now)
	shadow := bestCandidate(available, ranking)
	session := ""
	if opts.Metadata != nil {
		if id, ok := opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string); ok {
			session = truncateString(id, 48)
		}
	}
	s.state.recordDecision(ShadowDecision{
		At:       now,
		Provider: strings.ToLower(strings.TrimSpace(picked.Provider)),
		Model:    model,
		Session:  session,
		Live:     picked.ID,
		Shadow:   shadow.ID,
		Ranking:  ranking,
	})
	return picked, nil
}
