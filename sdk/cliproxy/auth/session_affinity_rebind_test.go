package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// rebindTestExecutor fails the next attempt on failID with a 503 and records
// which credential served the last successful attempt.
type rebindTestExecutor struct {
	schedulerTestExecutor
	failID string
	served string
}

func (e *rebindTestExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if auth.ID == e.failID {
		e.failID = ""
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "overloaded"}
	}
	e.served = auth.ID
	return cliproxyexecutor.Response{}, nil
}

func newRebindTestManager(t *testing.T, provider, model string, rebindOnQuota bool, executor ProviderExecutor) (*Manager, string, string) {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback:      &FillFirstSelector{},
		TTL:           time.Hour,
		RebindOnQuota: rebindOnQuota,
	})
	t.Cleanup(affinity.Stop)
	manager.SetSelector(affinity)
	manager.RegisterExecutor(executor)

	boundID, otherID := provider+"-a", provider+"-b"
	for _, id := range []string{boundID, otherID} {
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: provider, Status: StatusActive}); errRegister != nil {
			t.Fatalf("Register(%s): %v", id, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return manager, boundID, otherID
}

// rebindTestSessions builds the request for a conversation turn (1-based), either
// with an explicit session identity or as an LCP prompt-prefix session.
var rebindTestSessions = map[string]func(turn int) cliproxyexecutor.Options{
	"explicit": func(int) cliproxyexecutor.Options {
		return cliproxyexecutor.Options{Metadata: map[string]any{
			cliproxyexecutor.DerivedSessionIDMetadataKey: "stable-session",
		}}
	},
	"lcp": func(turn int) cliproxyexecutor.Options {
		messages := []string{`{"role":"system","content":"rebind test"}`}
		for i := 1; i <= turn; i++ {
			if i > 1 {
				messages = append(messages, fmt.Sprintf(`{"role":"assistant","content":"answer %d"}`, i-1))
			}
			messages = append(messages, fmt.Sprintf(`{"role":"user","content":"question %d"}`, i))
		}
		return cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAI,
			OriginalRequest: []byte(`{"messages":[` + strings.Join(messages, ",") + `]}`),
			Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "rebind-caller"},
		}
	},
}

// expireRebindTestCooldown ends every cooldown on authID, including an account-wide limit.
func expireRebindTestCooldown(t *testing.T, manager *Manager, authID, model string) {
	t.Helper()
	expireSessionAffinityPriorityModelCooldown(t, manager, authID, model)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	auth := manager.auths[authID]
	expired := time.Now().Add(-time.Second)
	auth.NextRetryAfter = expired
	auth.Quota.NextRecoverAt = expired
}

func TestSessionAffinityTransientCooldownByRebindMode(t *testing.T) {
	for session, sessionOpts := range rebindTestSessions {
		for _, testCase := range []struct {
			name          string
			rebindOnQuota bool
			// wantAfterRecovery serves the first turn after the bound credential recovers.
			wantAfterRecovery string
		}{
			{name: "quota keeps binding", rebindOnQuota: true, wantAfterRecovery: "a"},
			{name: "any moves binding", rebindOnQuota: false, wantAfterRecovery: "b"},
		} {
			t.Run(session+"/"+testCase.name, func(t *testing.T) {
				provider := fmt.Sprintf("rebind-transient-%s-%t", session, testCase.rebindOnQuota)
				model := "rebind-model"
				executor := &rebindTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: provider}}
				manager, boundID, otherID := newRebindTestManager(t, provider, model, testCase.rebindOnQuota, executor)
				ids := map[string]string{"a": boundID, "b": otherID}

				execute := func(label string, turn int) string {
					t.Helper()
					executor.served = ""
					if _, errExecute := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, sessionOpts(turn)); errExecute != nil {
						t.Fatalf("%s: Execute: %v", label, errExecute)
					}
					return executor.served
				}

				if got := execute("cold", 1); got != boundID {
					t.Fatalf("cold turn served by %q, want %q", got, boundID)
				}
				// The client resends the turn and the bound credential returns a 503;
				// the turn fails over transparently.
				executor.failID = boundID
				if got := execute("failover", 1); got != otherID {
					t.Fatalf("failover turn served by %q, want %q", got, otherID)
				}
				// While the bound credential cools down, the session is served elsewhere.
				if got := execute("during cooldown", 2); got != otherID {
					t.Fatalf("turn during cooldown served by %q, want %q", got, otherID)
				}
				expireSessionAffinityPriorityModelCooldown(t, manager, boundID, model)
				if got := execute("after recovery", 3); got != ids[testCase.wantAfterRecovery] {
					t.Fatalf("turn after recovery served by %q, want %q", got, ids[testCase.wantAfterRecovery])
				}
			})
		}
	}
}

func TestSessionAffinityRebindOnQuotaMovesOnAccountWideLimit(t *testing.T) {
	for session, sessionOpts := range rebindTestSessions {
		t.Run(session, func(t *testing.T) {
			ctx := context.Background()
			provider, model := "rebind-account-quota-"+session, "rebind-model"
			manager, boundID, otherID := newRebindTestManager(t, provider, model, true, schedulerTestExecutor{provider: provider})

			pick := func(turn int) string {
				t.Helper()
				auth, _, errPick := manager.pickNext(ctx, provider, model, sessionOpts(turn), nil)
				if errPick != nil {
					t.Fatalf("pickNext: %v", errPick)
				}
				return auth.ID
			}

			if got := pick(1); got != boundID {
				t.Fatalf("cold binding = %q, want %q", got, boundID)
			}
			// An account-wide 5h/7d rejection is a credential-scoped 429.
			manager.MarkResult(ctx, Result{
				AuthID:          boundID,
				Provider:        provider,
				Model:           model,
				Success:         false,
				CredentialScope: true,
				Error:           &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limit rejected"},
				Options:         sessionOpts(1),
			})
			if got := pick(2); got != otherID {
				t.Fatalf("pick after account-wide limit = %q, want %q", got, otherID)
			}
			expireRebindTestCooldown(t, manager, boundID, model)
			if got := pick(3); got != otherID {
				t.Fatalf("pick after the limit lifted = %q, want rebound %q", got, otherID)
			}
		})
	}
}

// A subagent inherits its parent's binding through the fallback alias. In quota
// mode a brief unavailability of the parent's credential must not bind the
// subagent elsewhere.
func TestSessionAffinityFallbackAliasByRebindMode(t *testing.T) {
	for _, testCase := range []struct {
		name              string
		rebindOnQuota     bool
		wantAfterRecovery string
	}{
		{name: "quota", rebindOnQuota: true, wantAfterRecovery: "auth-a"},
		{name: "any", rebindOnQuota: false, wantAfterRecovery: "auth-b"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
				Fallback:      &FillFirstSelector{},
				TTL:           time.Hour,
				RebindOnQuota: testCase.rebindOnQuota,
			})
			defer selector.Stop()
			authA, authB := &Auth{ID: "auth-a"}, &Auth{ID: "auth-b"}
			auths := []*Auth{authA, authB}
			ctx := context.WithValue(context.Background(), sessionAuthLookupKey{}, func(id string) (*Auth, bool) {
				for _, auth := range auths {
					if auth.ID == id {
						return auth, true
					}
				}
				return nil, false
			})
			pick := func(label string, headers http.Header) string {
				t.Helper()
				auth, errPick := selector.Pick(ctx, "claude", "model", cliproxyexecutor.Options{Headers: headers, Metadata: map[string]any{}}, auths)
				if errPick != nil {
					t.Fatalf("%s: %v", label, errPick)
				}
				return auth.ID
			}
			parent := http.Header{"X-Claude-Code-Session-Id": []string{"rebind-root"}}
			subagent := http.Header{"X-Claude-Code-Session-Id": []string{"rebind-root"}, "X-Claude-Code-Agent-Id": []string{"sub-1"}}

			if got := pick("parent", parent); got != "auth-a" {
				t.Fatalf("parent = %q, want auth-a", got)
			}
			authA.Unavailable = true // a transient, non-quota outage
			if got := pick("subagent during outage", subagent); got != "auth-b" {
				t.Fatalf("subagent during outage = %q, want auth-b", got)
			}
			authA.Unavailable = false
			if got := pick("subagent after recovery", subagent); got != testCase.wantAfterRecovery {
				t.Fatalf("subagent after recovery = %q, want %q", got, testCase.wantAfterRecovery)
			}
		})
	}
}
