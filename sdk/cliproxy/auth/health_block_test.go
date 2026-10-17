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

// midStreamHubExecutor answers the hub stream with one chunk, then stalls until
// its attempt ends and, like the Claude executor, races its error against the
// attempt context, so the error may or may not reach the conductor.
type midStreamHubExecutor struct {
	hangingHubExecutor
	streamCtx chan context.Context
}

func (e *midStreamHubExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if auth.ID != e.hubID {
		return e.hangingHubExecutor.ExecuteStream(ctx, auth, req, opts)
	}
	e.streamCtx <- ctx
	out := make(chan cliproxyexecutor.StreamChunk, 1)
	out <- cliproxyexecutor.StreamChunk{Payload: []byte("part1")}
	go func() {
		defer close(out)
		<-ctx.Done()
		select {
		case out <- cliproxyexecutor.StreamChunk{Err: ctx.Err()}:
		case <-ctx.Done():
		}
	}()
	return &cliproxyexecutor.StreamResult{Chunks: out}, nil
}

// After the first byte, a health block ends the stream with exactly one
// ErrHealthBlocked chunk and records the attempt as a failure, never a success.
// A stream that just received a chunk is spared, and a client that left gets
// no health error.
func TestSetHealthBlockEndsCutStreamWithError(t *testing.T) {
	hook := &resultCaptureHook{}
	m := NewManager(nil, nil, hook)
	hub := &Auth{ID: "health-midstream-hub", Provider: "claude", Status: StatusActive,
		Attributes: map[string]string{"priority": "100", "health_check": "true"}}
	fallback := &Auth{ID: "health-midstream-fallback", Provider: "claude", Status: StatusActive}
	reg := registry.GetGlobalRegistry()
	for _, a := range []*Auth{hub, fallback} {
		reg.RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: "claude-3"}})
		t.Cleanup(func() { reg.UnregisterClient(a.ID) })
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatalf("register %s: %v", a.ID, err)
		}
	}
	exec := &midStreamHubExecutor{hangingHubExecutor: hangingHubExecutor{hubID: hub.ID}, streamCtx: make(chan context.Context, 1)}
	m.RegisterExecutor(exec)
	start := func(ctx context.Context) (<-chan cliproxyexecutor.StreamChunk, context.Context) {
		result, err := m.ExecuteStream(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: "claude-3"}, cliproxyexecutor.Options{})
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if chunk := <-result.Chunks; string(chunk.Payload) != "part1" || chunk.Err != nil {
			t.Fatalf("first chunk = %q, %v; want part1 from the hub", chunk.Payload, chunk.Err)
		}
		return result.Chunks, <-exec.streamCtx
	}
	block := func() { m.SetHealthBlock(hub.ID, time.Now().Add(time.Minute)) }
	previous := healthStreamGrace
	t.Cleanup(func() { healthStreamGrace = previous })

	// A client that leaves gets no health error, and its credential is not
	// cooled: the next stream still starts on the hub.
	ctx, cancel := context.WithCancel(context.Background())
	chunks, _ := start(ctx)
	cancel()
	healthStreamGrace = 0
	block()
	for chunk := range chunks {
		if errors.Is(chunk.Err, ErrHealthBlocked) {
			t.Fatal("client that left got a health error")
		}
	}
	m.SetHealthBlock(hub.ID, time.Time{})
	healthStreamGrace = previous

	before := len(hook.Results())
	chunks, attemptCtx := start(context.Background())
	block()
	if attemptCtx.Err() != nil {
		t.Fatal("a stream that just received a chunk was cut by the block")
	}
	healthStreamGrace = 0 // the stream has now been idle past the grace
	block()
	var errs []error
	for chunk := range chunks {
		if chunk.Err == nil {
			t.Fatalf("payload %q after the cut", chunk.Payload)
		}
		errs = append(errs, chunk.Err)
	}
	if len(errs) != 1 || !errors.Is(errs[0], ErrHealthBlocked) {
		t.Fatalf("stream ended with errors %v, want exactly one ErrHealthBlocked", errs)
	}
	failed := false
	for _, r := range hook.Results()[before:] {
		if r.AuthID != hub.ID {
			continue
		}
		if r.Success {
			t.Fatal("cut stream recorded as a success")
		}
		failed = failed || (r.Error != nil && r.Error.Message == ErrHealthBlocked.Error())
	}
	if !failed {
		t.Fatalf("cut stream not recorded as a failure: %+v", hook.Results())
	}
}

// With session affinity off the scheduler picks, and a health block is a
// cooldown that ends at HealthBlockedUntil: when the checker stops (no probe
// lifts the block), the hub comes back on its own once the block expires.
func TestHealthBlockExpiresOnSchedulerPath(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	m.SetSelector(&FillFirstSelector{})
	const provider, model = "health-expiry", "health-expiry-model"
	m.RegisterExecutor(schedulerTestExecutor{provider: provider})
	hubID, localID := "health-expiry-hub", "health-expiry-local"
	for id, attrs := range map[string]map[string]string{
		hubID:   {"priority": "100", "health_check": "true"},
		localID: {"priority": "1"},
	} {
		if _, err := m.Register(WithSkipPersist(ctx), &Auth{ID: id, Provider: provider, Status: StatusActive, Attributes: attrs}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	pick := func() string {
		t.Helper()
		auth, _, err := m.pickNext(ctx, provider, model, cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatalf("pickNext: %v", err)
		}
		return auth.ID
	}

	if got := pick(); got != hubID {
		t.Fatalf("before the block picked %q, want the hub", got)
	}
	until := time.Now().Add(150 * time.Millisecond)
	m.SetHealthBlock(hubID, until)
	if got := pick(); got != localID {
		t.Fatalf("during the block picked %q, want %q", got, localID)
	}
	time.Sleep(time.Until(until) + 50*time.Millisecond)
	if got := pick(); got != hubID {
		t.Fatalf("after the block expired picked %q, want the hub back", got)
	}
}
