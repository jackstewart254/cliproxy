package auth

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// ErrHealthBlocked ends an attempt that was in flight on a credential when the
// endpoint health check took it out of rotation. The conductor treats it as a
// credential failure and moves on to the next credential in the same request.
var ErrHealthBlocked = errors.New("upstream endpoint stopped answering its health check")

// healthStreamGrace spares a stream that received a chunk this recently: its
// endpoint is still answering, so a health check blip must not cut it. It stays
// below the shortest time from an endpoint hanging to its block (one interval
// plus one timeout, 15s), so every stream on a hung endpoint is still cut.
var healthStreamGrace = 10 * time.Second

type healthAttempt struct {
	cancel    context.CancelCauseFunc
	client    context.Context // the request context the attempt derives from
	lastChunk atomic.Int64    // unix nanos of the last stream chunk, 0 before one
}

type healthAttemptKey struct{}

// SetHealthBlock blocks every model of an auth until the given time, or lifts
// the block when until is zero. Blocking also cancels the attempts in flight on
// that auth with ErrHealthBlocked, so requests stuck on an endpoint that never
// answers fail over instead of waiting for the client to give up. A stream that
// is still receiving chunks is spared; each failed round blocks again and so
// re-checks it. The block is separate from request cooldowns, so lifting it
// never clears a 401 or quota.
func (m *Manager) SetHealthBlock(authID string, until time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil || auth.HealthBlockedUntil.Equal(until) {
		m.mu.Unlock()
		return
	}
	auth.HealthBlockedUntil = until
	auth.Generation++
	auth.UpdatedAt = time.Now()
	snapshot := auth.Clone()
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuthResult(snapshot, nil, true)
	}
	now := time.Now()
	if !until.After(now) {
		return
	}
	m.healthMu.Lock()
	attempts := make([]*healthAttempt, 0, len(m.healthAttempts[authID]))
	for attempt := range m.healthAttempts[authID] {
		attempts = append(attempts, attempt)
	}
	m.healthMu.Unlock()
	for _, attempt := range attempts {
		if last := attempt.lastChunk.Load(); last != 0 && now.Sub(time.Unix(0, last)) < healthStreamGrace {
			continue
		}
		attempt.cancel(ErrHealthBlocked)
	}
}

// trackHealthAttempt makes an attempt on a health-checked credential
// cancellable by SetHealthBlock. Other credentials get ctx back unchanged. The
// entry is dropped when the attempt context ends, which is at the latest when
// the request finishes; a stream stays cancellable while it is being read.
func (m *Manager) trackHealthAttempt(ctx context.Context, auth *Auth) context.Context {
	if m == nil || ctx == nil || auth == nil || auth.Attributes["health_check"] != "true" {
		return ctx
	}
	attempt := &healthAttempt{client: ctx}
	ctx, attempt.cancel = context.WithCancelCause(ctx)
	ctx = context.WithValue(ctx, healthAttemptKey{}, attempt)
	m.healthMu.Lock()
	if m.healthAttempts == nil {
		m.healthAttempts = make(map[string]map[*healthAttempt]struct{})
	}
	if m.healthAttempts[auth.ID] == nil {
		m.healthAttempts[auth.ID] = make(map[*healthAttempt]struct{})
	}
	m.healthAttempts[auth.ID][attempt] = struct{}{}
	m.healthMu.Unlock()
	context.AfterFunc(ctx, func() {
		m.healthMu.Lock()
		delete(m.healthAttempts[auth.ID], attempt)
		if len(m.healthAttempts[auth.ID]) == 0 {
			delete(m.healthAttempts, auth.ID)
		}
		m.healthMu.Unlock()
	})
	return ctx
}

// healthAttemptFrom returns the tracked attempt ctx belongs to, or nil.
func healthAttemptFrom(ctx context.Context) *healthAttempt {
	if ctx == nil {
		return nil
	}
	attempt, _ := ctx.Value(healthAttemptKey{}).(*healthAttempt)
	return attempt
}

// touch records that the attempt's stream just received a chunk.
func (a *healthAttempt) touch() {
	if a != nil {
		a.lastChunk.Store(time.Now().UnixNano())
	}
}

// cut reports whether SetHealthBlock ended ctx's attempt while its client is
// still there to be told; a client that left is not.
func (a *healthAttempt) cut(ctx context.Context) bool {
	return a != nil && healthAborted(ctx) && a.client.Err() == nil
}

// healthAborted reports whether ctx was cancelled by SetHealthBlock rather
// than by the client.
func healthAborted(ctx context.Context) bool {
	return ctx != nil && errors.Is(context.Cause(ctx), ErrHealthBlocked)
}
