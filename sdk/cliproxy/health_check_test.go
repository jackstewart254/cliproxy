package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type hungHubExecutor struct {
	hubID    string
	inFlight chan struct{}
}

func (e hungHubExecutor) Identifier() string { return "claude" }
func (e hungHubExecutor) Execute(ctx context.Context, auth *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if auth.ID == e.hubID {
		select {
		case e.inFlight <- struct{}{}:
		default:
		}
		<-ctx.Done() // a hub that accepted the request and never answers
		return cliproxyexecutor.Response{}, ctx.Err()
	}
	return cliproxyexecutor.Response{Payload: []byte("fallback")}, nil
}
func (e hungHubExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}
func (e hungHubExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}
func (e hungHubExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}
func (e hungHubExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

// A hub that hangs on /v1/messages, with a /healthz that hangs and then fails,
// is taken out of rotation after two rounds: the request stuck on it and new
// requests go to the fallback; one healthy round puts it back.
func TestHealthCheckBlocksHungHubAndLiftsOnRecovery(t *testing.T) {
	previous := healthCheckTimeout
	healthCheckTimeout = 100 * time.Millisecond
	t.Cleanup(func() { healthCheckTimeout = previous })

	var mode atomic.Value // "hang", "fail" or "ok"
	mode.Store("hang")
	healthz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case "hang":
			<-r.Context().Done()
		case "fail":
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	t.Cleanup(healthz.Close)

	ctx := context.Background()
	manager := coreauth.NewManager(nil, nil, nil)
	hub := &coreauth.Auth{ID: "health-hub", Provider: "claude", Status: coreauth.StatusActive,
		Attributes: map[string]string{"priority": "100", "base_url": healthz.URL, "health_check": "true"}}
	fallback := &coreauth.Auth{ID: "health-fallback", Provider: "claude", Status: coreauth.StatusActive}
	reg := registry.GetGlobalRegistry()
	for _, a := range []*coreauth.Auth{hub, fallback} {
		reg.RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: "claude-3"}})
		t.Cleanup(func() { reg.UnregisterClient(a.ID) })
		if _, err := manager.Register(ctx, a); err != nil {
			t.Fatalf("register %s: %v", a.ID, err)
		}
	}
	exec := hungHubExecutor{hubID: hub.ID, inFlight: make(chan struct{}, 1)}
	manager.RegisterExecutor(exec)
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	served := func(timeout time.Duration) string {
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		resp, err := manager.Execute(reqCtx, []string{"claude"}, cliproxyexecutor.Request{Model: "claude-3"}, cliproxyexecutor.Options{})
		if err != nil {
			return "error: " + err.Error()
		}
		return string(resp.Payload)
	}

	failures := make(map[string]int)
	service.checkHealth(ctx, failures) // healthz hangs: failure 1, still in rotation
	if got := served(200 * time.Millisecond); got == "fallback" {
		t.Fatalf("one failed round already moved traffic: %q", got)
	}
	// A request stuck on the hub when the block engages moves to the fallback.
	select {
	case <-exec.inFlight: // the timed-out request above
	default:
	}
	stuck := make(chan string, 1)
	go func() { stuck <- served(10 * time.Second) }()
	<-exec.inFlight
	mode.Store("fail")
	service.checkHealth(ctx, failures) // healthz 502: failure 2, blocked
	if got := <-stuck; got != "fallback" {
		t.Fatalf("in-flight request got %q after the block, want fallback", got)
	}
	if got := served(5 * time.Second); got != "fallback" {
		t.Fatalf("after two failed rounds got %q, want fallback", got)
	}

	mode.Store("ok")
	service.checkHealth(ctx, failures)
	if a, _ := manager.GetByID(hub.ID); !a.HealthBlockedUntil.IsZero() {
		t.Fatalf("block not lifted after a healthy round: %v", a.HealthBlockedUntil)
	}
	if got := served(200 * time.Millisecond); got == "fallback" {
		t.Fatalf("hub not back in rotation: %q", got)
	}
}
