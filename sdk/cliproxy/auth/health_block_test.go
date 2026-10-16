package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// hangingHubExecutor never answers on the hub credential (it waits for its
// attempt context) and answers at once on every other credential.
type hangingHubExecutor struct {
	hubID    string
	inFlight chan struct{}
}

func (e *hangingHubExecutor) Identifier() string { return "claude" }

func (e *hangingHubExecutor) hang(ctx context.Context, auth *Auth) error {
	if auth.ID != e.hubID {
		return nil
	}
	e.inFlight <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

func (e *hangingHubExecutor) Execute(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.hang(ctx, auth); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte("fallback")}, nil
}

func (e *hangingHubExecutor) ExecuteStream(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := e.hang(ctx, auth); err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("fallback")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *hangingHubExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e *hangingHubExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e *hangingHubExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

// A request already stuck on a health-checked hub moves on to the fallback in
// the same request once the health check blocks the hub, for plain, streaming
// and count_tokens calls. A client cancel still ends the request instead.
func TestSetHealthBlockFailsOverInFlightAttempts(t *testing.T) {
	m := NewManager(nil, nil, nil)
	hub := &Auth{ID: "health-inflight-hub", Provider: "claude", Status: StatusActive,
		Attributes: map[string]string{"priority": "100", "health_check": "true"}}
	fallback := &Auth{ID: "health-inflight-fallback", Provider: "claude", Status: StatusActive}
	reg := registry.GetGlobalRegistry()
	for _, a := range []*Auth{hub, fallback} {
		reg.RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: "claude-3"}})
		t.Cleanup(func() { reg.UnregisterClient(a.ID) })
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatalf("register %s: %v", a.ID, err)
		}
	}
	exec := &hangingHubExecutor{hubID: hub.ID, inFlight: make(chan struct{}, 1)}
	m.RegisterExecutor(exec)
	req := cliproxyexecutor.Request{Model: "claude-3"}
	calls := map[string]func(context.Context) (string, error){
		"execute": func(ctx context.Context) (string, error) {
			resp, err := m.Execute(ctx, []string{"claude"}, req, cliproxyexecutor.Options{})
			return string(resp.Payload), err
		},
		"count": func(ctx context.Context) (string, error) {
			resp, err := m.ExecuteCount(ctx, []string{"claude"}, req, cliproxyexecutor.Options{})
			return string(resp.Payload), err
		},
		"stream": func(ctx context.Context) (string, error) {
			result, err := m.ExecuteStream(ctx, []string{"claude"}, req, cliproxyexecutor.Options{})
			if err != nil {
				return "", err
			}
			var out []byte
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					return "", chunk.Err
				}
				out = append(out, chunk.Payload...)
			}
			return string(out), nil
		},
	}
	run := func(name string, ctx context.Context, interrupt func()) (string, error) {
		type outcome struct {
			body string
			err  error
		}
		done := make(chan outcome, 1)
		go func() {
			body, err := calls[name](ctx)
			done <- outcome{body, err}
		}()
		select {
		case <-exec.inFlight:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: hub attempt never started", name)
		}
		interrupt()
		select {
		case got := <-done:
			return got.body, got.err
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: request still hung after the interrupt", name)
			return "", nil
		}
	}

	for name := range calls {
		m.SetHealthBlock(hub.ID, time.Time{})
		body, err := run(name, context.Background(), func() { m.SetHealthBlock(hub.ID, time.Now().Add(time.Minute)) })
		if err != nil || body != "fallback" {
			t.Fatalf("%s after health block: body=%q err=%v, want fallback", name, body, err)
		}
	}

	m.SetHealthBlock(hub.ID, time.Time{})
	ctx, cancel := context.WithCancel(context.Background())
	if body, err := run("execute", ctx, cancel); !errors.Is(err, context.Canceled) || body == "fallback" {
		t.Fatalf("client cancel: body=%q err=%v, want context.Canceled and no fallback", body, err)
	}
}
