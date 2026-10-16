package auth

import (
	"context"
	"errors"
	"time"
)

// ErrHealthBlocked ends an attempt that was in flight on a credential when the
// endpoint health check took it out of rotation. The conductor treats it as a
// credential failure and moves on to the next credential in the same request.
var ErrHealthBlocked = errors.New("upstream endpoint stopped answering its health check")

type healthAttempt struct{ cancel context.CancelCauseFunc }

// SetHealthBlock blocks every model of an auth until the given time, or lifts
// the block when until is zero. Blocking also cancels the attempts in flight on
// that auth with ErrHealthBlocked, so requests stuck on an endpoint that never
// answers fail over instead of waiting for the client to give up. The block is
// separate from request cooldowns, so lifting it never clears a 401 or quota.
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
	if !until.After(time.Now()) {
		return
	}
	m.healthMu.Lock()
	attempts := make([]*healthAttempt, 0, len(m.healthAttempts[authID]))
	for attempt := range m.healthAttempts[authID] {
		attempts = append(attempts, attempt)
	}
	m.healthMu.Unlock()
	for _, attempt := range attempts {
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
	ctx, cancel := context.WithCancelCause(ctx)
	attempt := &healthAttempt{cancel: cancel}
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

// healthAborted reports whether ctx was cancelled by SetHealthBlock rather
// than by the client.
func healthAborted(ctx context.Context) bool {
	return ctx != nil && errors.Is(context.Cause(ctx), ErrHealthBlocked)
}
