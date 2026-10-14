package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
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

func rebindTestSessionOpts() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.DerivedSessionIDMetadataKey: "stable-session",
	}}
}

func rebindTestBinding(t *testing.T, manager *Manager, provider, model string) string {
	t.Helper()
	auth, status := manager.LookupSessionAffinity(provider, model, "derived:stable-session")
	if status != "bound" || auth == nil {
		t.Fatalf("session binding status = %q, want bound", status)
	}
	return auth.ID
}

func TestSessionAffinityTransientCooldownByRebindMode(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		rebindOnQuota bool
		// wantBinding is the session binding after the bound credential fails over.
		wantBinding string
		// wantAfterRecovery serves the first request after the bound credential recovers.
		wantAfterRecovery string
	}{
		{name: "quota keeps binding", rebindOnQuota: true, wantBinding: "a", wantAfterRecovery: "a"},
		{name: "any moves binding", rebindOnQuota: false, wantBinding: "b", wantAfterRecovery: "b"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			provider := "rebind-transient-" + map[bool]string{true: "quota", false: "any"}[testCase.rebindOnQuota]
			model := "rebind-model"
			executor := &rebindTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: provider}}
			manager, boundID, otherID := newRebindTestManager(t, provider, model, testCase.rebindOnQuota, executor)
			ids := map[string]string{"a": boundID, "b": otherID}

			execute := func(label string) string {
				t.Helper()
				executor.served = ""
				if _, errExecute := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, rebindTestSessionOpts()); errExecute != nil {
					t.Fatalf("%s: Execute: %v", label, errExecute)
				}
				return executor.served
			}

			if got := execute("cold"); got != boundID {
				t.Fatalf("cold request served by %q, want %q", got, boundID)
			}

			// The bound credential returns a 503; the request fails over transparently.
			executor.failID = boundID
			if got := execute("failover"); got != otherID {
				t.Fatalf("failover request served by %q, want %q", got, otherID)
			}
			if got := rebindTestBinding(t, manager, provider, model); got != ids[testCase.wantBinding] {
				t.Fatalf("binding after failover = %q, want %q", got, ids[testCase.wantBinding])
			}

			// While the bound credential cools down, the session is still served elsewhere.
			if got := execute("during cooldown"); got != otherID {
				t.Fatalf("request during cooldown served by %q, want %q", got, otherID)
			}
			if got := rebindTestBinding(t, manager, provider, model); got != ids[testCase.wantBinding] {
				t.Fatalf("binding during cooldown = %q, want %q", got, ids[testCase.wantBinding])
			}

			expireSessionAffinityPriorityModelCooldown(t, manager, boundID, model)
			if got := execute("after recovery"); got != ids[testCase.wantAfterRecovery] {
				t.Fatalf("request after recovery served by %q, want %q", got, ids[testCase.wantAfterRecovery])
			}
		})
	}
}

func TestSessionAffinityRebindOnQuotaMovesOnAccountWideLimit(t *testing.T) {
	ctx := context.Background()
	provider, model := "rebind-account-quota", "rebind-model"
	manager, boundID, otherID := newRebindTestManager(t, provider, model, true, schedulerTestExecutor{provider: provider})

	pick := func() string {
		t.Helper()
		auth, _, errPick := manager.pickNext(ctx, provider, model, rebindTestSessionOpts(), nil)
		if errPick != nil {
			t.Fatalf("pickNext: %v", errPick)
		}
		return auth.ID
	}

	if got := pick(); got != boundID {
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
		Options:         rebindTestSessionOpts(),
	})
	if got := pick(); got != otherID {
		t.Fatalf("pick after account-wide limit = %q, want %q", got, otherID)
	}
	if got := rebindTestBinding(t, manager, provider, model); got != otherID {
		t.Fatalf("binding after account-wide limit = %q, want rebind to %q", got, otherID)
	}
}
