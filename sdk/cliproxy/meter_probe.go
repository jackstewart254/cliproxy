package cliproxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// claudeUsageURL is a variable so tests can point it at a local server.
var claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

const (
	// meterProbeInterval is how often accounts are checked, so a newly added
	// account shows its allowance within a minute. Only accounts without a
	// reading newer than meterProbeMaxAge are probed: those that carry traffic
	// read their meters from responses, and idle ones every 20 minutes.
	meterProbeInterval = time.Minute
	meterProbeFirstRun = time.Minute
	meterProbeMaxAge   = 20 * time.Minute
)

// startClaudeMeterProbe reads the usage of signed-in Claude accounts that have
// no recent meter reading, so idle accounts show and rank by real allowance.
func (s *Service) startClaudeMeterProbe(ctx context.Context) {
	var running atomic.Bool
	run := func() {
		// A probe has no timeout, so a hung request must not stack rounds.
		if !running.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer running.Store(false)
			s.probeClaudeMeters(ctx)
		}()
	}
	go func() {
		timer := time.NewTimer(meterProbeFirstRun)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				run()
				timer.Reset(meterProbeInterval)
			}
		}
	}()
}

func (s *Service) probeClaudeMeters(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	state := coreauth.DefaultRoutingState()
	for _, auth := range s.coreManager.List() {
		if ctx.Err() != nil {
			return
		}
		if auth == nil || auth.Disabled || !strings.EqualFold(auth.Provider, "claude") {
			continue
		}
		token := claudeauth.ReadMetadataString(&auth.Metadata, "access_token")
		if !strings.Contains(token, "sk-ant-oat") || !state.NeedsMeterReading(auth.ID, time.Now(), meterProbeMaxAge) {
			continue
		}
		body, errProbe := s.fetchClaudeUsage(ctx, auth, token)
		now := time.Now()
		if errProbe != nil {
			warnProbeFailure(auth.ID, errProbe, now)
			continue
		}
		probeWarnings.Delete(auth.ID)
		if meters := coreauth.ParseClaudeUsageMeters(body, now); len(meters) > 0 {
			state.ObserveMeters(auth.ID, "claude", meters, now)
		}
	}
}

func (s *Service) fetchClaudeUsage(ctx context.Context, auth *coreauth.Auth, token string) ([]byte, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", helps.DefaultClaudeUserAgent(s.cfg))
	resp, errDo := helps.NewProxyAwareHTTPClient(ctx, s.cfg, auth, 0).Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("claude usage probe: close body: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, errRead
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &usageProbeStatusError{status: resp.StatusCode}
	}
	return body, nil
}

// probeWarnings holds when each account's probe failure was last warned
// about. A failing account has no fresh reading, so it is probed again every
// minute; it is warned about at most once per meterProbeMaxAge, whatever the
// error says (network errors name a new local port each time).
var probeWarnings sync.Map // auth ID -> time.Time

// warnProbeFailure logs a failed probe at Warn, or at Debug when the account
// was warned about within meterProbeMaxAge. It reports whether it warned.
func warnProbeFailure(authID string, errProbe error, now time.Time) bool {
	if last, ok := probeWarnings.Load(authID); ok && now.Sub(last.(time.Time)) < meterProbeMaxAge {
		log.WithField("auth", authID).Debugf("claude usage probe: %v", errProbe)
		return false
	}
	probeWarnings.Store(authID, now)
	log.WithField("auth", authID).Warnf("claude usage probe failed, keeping the last reading: %v", errProbe)
	return true
}

type usageProbeStatusError struct{ status int }

func (e *usageProbeStatusError) Error() string {
	return "usage endpoint returned " + http.StatusText(e.status)
}
