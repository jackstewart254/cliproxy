package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// An idle Claude account has no reading until traffic reaches it. The probe
// reads its usage so the dashboard and router see its allowance at once.
func TestClaudeMeterProbeReadsIdleOAuthAccountsOnly(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			t.Errorf("anthropic-beta = %q", r.Header.Get("anthropic-beta"))
		}
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":0.0,"resets_at":null},"seven_day":{"utilization":3.0,"resets_at":"2026-10-11T14:00:00+00:00"}}`))
	}))
	defer server.Close()
	previous := claudeUsageURL
	claudeUsageURL = server.URL
	defer func() { claudeUsageURL = previous }()

	ctx := context.Background()
	manager := coreauth.NewManager(nil, nil, nil)
	register := func(id string, disabled bool, token string) {
		auth := &coreauth.Auth{ID: id, Provider: "claude", Disabled: disabled, Metadata: map[string]any{"access_token": token}}
		if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
			t.Fatalf("register %s: %v", id, errRegister)
		}
	}
	register("probe-idle.json", false, "sk-ant-oat01-idle")
	register("probe-off.json", true, "sk-ant-oat01-off")
	register("probe-apikey.json", false, "sk-ant-api03-key")
	service := &Service{cfg: &config.Config{}, coreManager: manager}

	service.probeClaudeMeters(ctx)

	if len(seen) != 1 || seen[0] != "Bearer sk-ant-oat01-idle" {
		t.Fatalf("probed %v, want only the enabled OAuth account", seen)
	}
	state := coreauth.DefaultRoutingState()
	if state.NeedsMeterReading("probe-idle.json", time.Now(), meterProbeMaxAge) {
		t.Fatal("the probe did not record a reading")
	}
	// A fresh reading is not probed again.
	service.probeClaudeMeters(ctx)
	if len(seen) != 1 {
		t.Fatalf("probed again with a fresh reading: %v", seen)
	}
}

// A probe that keeps failing is retried every minute; it is warned about once
// per meterProbeMaxAge, or again when the error changes.
func TestWarnProbeFailureIsRateLimited(t *testing.T) {
	const id = "probe-warn.json"
	defer probeWarnings.Delete(id)
	start := time.Now()
	same, other := errors.New("usage endpoint returned Unauthorized"), errors.New("connection refused")
	steps := []struct {
		err  error
		at   time.Duration
		want bool
	}{
		{same, 0, true},
		{same, time.Minute, false},
		{other, 2 * time.Minute, true},
		{other, 2*time.Minute + meterProbeMaxAge, true},
	}
	for i, step := range steps {
		if got := warnProbeFailure(id, step.err, start.Add(step.at)); got != step.want {
			t.Fatalf("step %d: warned = %v, want %v", i, got, step.want)
		}
	}
}
