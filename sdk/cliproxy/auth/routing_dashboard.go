package auth

import (
	"sort"
	"strings"
	"time"
)

// RouterView is what the dashboard shows about routing: each credential's
// meters and health, how the scored selector would rank a new session now,
// and how its shadow picks compared with the live ones.
type RouterView struct {
	Mode        string                       `json:"mode"`
	Headroom    map[string]float64           `json:"headroom,omitempty"`
	MaxSessions int                          `json:"max_sessions"`
	Accounts    map[string]RouterAccountView `json:"accounts"`
	Rankings    []RouterRanking              `json:"rankings,omitempty"`
	Shadow      *ShadowSummary               `json:"shadow,omitempty"`
}

// RouterAccountView is one credential's meters and health.
type RouterAccountView struct {
	Meters         []MeterView `json:"meters"`
	BindingClaim   string      `json:"binding_claim,omitempty"`
	OverageStatus  string      `json:"overage_status,omitempty"`
	Refused        string      `json:"refused,omitempty"`
	ActiveSessions int         `json:"active_sessions"`
	ObservedAt     time.Time   `json:"observed_at,omitempty"`
}

// MeterView is one meter as the dashboard draws it.
type MeterView struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	// Utilization is null while the meter has no known used share.
	Utilization   *float64  `json:"utilization"`
	ResetAt       time.Time `json:"reset_at,omitempty"`
	WindowSeconds int64     `json:"window_seconds,omitempty"`
	Status        string    `json:"status,omitempty"`
	Models        []string  `json:"models,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
	// ResetSinceReading reports that the window reset after the last reading,
	// so Utilization is the fresh window's zero.
	ResetSinceReading bool     `json:"reset_since_reading,omitempty"`
	BurnPerHour       *float64 `json:"burn_per_hour,omitempty"`
	Long              bool     `json:"long"`
}

// RouterRanking is the scored selector's ranking for one provider.
type RouterRanking struct {
	Provider   string           `json:"provider"`
	Model      string           `json:"model,omitempty"`
	Candidates []RouteCandidate `json:"candidates"`
}

// Dashboard builds the dashboard view for the given credentials.
func (s *RoutingState) Dashboard(auths []*Auth, now time.Time) RouterView {
	mode, cfg := s.Scorer()
	scorer := NewScoredSelector(cfg, s)
	view := RouterView{
		Mode:        mode,
		Headroom:    scorer.cfg.Headroom,
		MaxSessions: scorer.cfg.maxSessions(),
		Accounts:    make(map[string]RouterAccountView),
	}
	sessions := s.activeSessions(now)
	byProvider := make(map[string][]*Auth)
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		byProvider[provider] = append(byProvider[provider], auth)
		account := s.account(auth.ID)
		if account == nil {
			continue
		}
		accountView := RouterAccountView{
			BindingClaim:   account.Facts.BindingClaim,
			OverageStatus:  account.Facts.OverageStatus,
			ActiveSessions: sessions[auth.ID],
			ObservedAt:     account.Facts.ObservedAt,
		}
		if reason, refused := account.Health.refusedReason(now); refused {
			accountView.Refused = capitalize(reason)
		}
		for _, meter := range account.Meters {
			effective, wasReset := effectiveMeter(*meter, now)
			meterView := MeterView{
				Name:              meter.Name,
				Title:             meterTitle(provider, meter),
				Utilization:       knownUtilization(effective),
				ResetAt:           effective.ResetAt,
				WindowSeconds:     int64(meter.Window / time.Second),
				Status:            effective.Status,
				Models:            meter.Models,
				ObservedAt:        meter.ObservedAt,
				ResetSinceReading: wasReset,
				Long:              meterIsLong(meter),
			}
			if !wasReset {
				if rate, known := burnRate(account.History[meter.Name], now, meterBurnLookback(meter)); known {
					meterView.BurnPerHour = &rate
				}
			}
			accountView.Meters = append(accountView.Meters, meterView)
		}
		sort.Slice(accountView.Meters, func(i, j int) bool {
			return meterOrder(accountView.Meters[i]) < meterOrder(accountView.Meters[j])
		})
		view.Accounts[auth.ID] = accountView
	}
	if mode == "" || mode == "off" {
		return view
	}
	providers := make([]string, 0, len(byProvider))
	for provider := range byProvider {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	for _, provider := range providers {
		candidates := byProvider[provider]
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
		model := s.lastModelFor(provider)
		view.Rankings = append(view.Rankings, RouterRanking{
			Provider:   provider,
			Model:      model,
			Candidates: scorer.Rank(model, candidates, now),
		})
	}
	summary := s.shadowSummary(now, 10)
	view.Shadow = &summary
	return view
}

// knownUtilization is the meter's used share, or nil when it is unknown.
func knownUtilization(meter Meter) *float64 {
	if meter.UtilizationUnknown {
		return nil
	}
	return &meter.Utilization
}

// meterOrder puts the 5-hour window first, the shared week second and any
// per-model windows after them.
func meterOrder(meter MeterView) string {
	switch meter.Name {
	case "5h", "primary":
		return "0"
	case "7d", "secondary":
		return "1"
	}
	if meter.Long {
		return "3" + meter.Name
	}
	return "2" + meter.Name
}
