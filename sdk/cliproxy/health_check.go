package cliproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Endpoint health check for API-key credentials with `health-check: true`.
// Requests to an endpoint that accepts the connection but never answers cannot
// be timed out (no timeouts after connect), so this checks the endpoint off the
// request path instead: GET {base-url}/healthz every healthCheckInterval. After
// healthCheckFailures failures in a row the credential is blocked for all
// models, so new requests skip it: at most 2*interval+timeout (27s) after the
// endpoint stops answering. The first success lifts the block. The
// block outlives two missed rounds and then expires by itself, so a stopped
// checker never leaves a credential blocked.
var (
	healthCheckInterval = 12 * time.Second
	healthCheckTimeout  = 3 * time.Second
)

const healthCheckFailures = 2

// startHealthCheck runs one checker goroutine until ctx ends (service
// shutdown). Each round reads the current credentials, so a config reload
// takes effect on the next round without restarting it.
func (s *Service) startHealthCheck(ctx context.Context) {
	go func() {
		failures := make(map[string]int)
		ticker := time.NewTicker(healthCheckInterval)
		defer ticker.Stop()
		for {
			s.checkHealth(ctx, failures)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// checkHealth probes every opted-in credential once, sequentially.
// ponytail: sequential probes, a round takes up to healthCheckTimeout per
// unhealthy endpoint; probe in parallel if many credentials opt in.
func (s *Service) checkHealth(ctx context.Context, failures map[string]int) {
	if s == nil || s.coreManager == nil {
		return
	}
	checked := make(map[string]bool)
	for _, auth := range s.coreManager.List() {
		if auth == nil || auth.Disabled || auth.Attributes["health_check"] != "true" {
			continue
		}
		base := strings.TrimRight(strings.TrimSpace(auth.Attributes["base_url"]), "/")
		if base == "" {
			continue
		}
		checked[auth.ID] = true
		errProbe := s.probeHealth(ctx, auth, base+"/healthz")
		if ctx.Err() != nil {
			return
		}
		if errProbe == nil {
			if failures[auth.ID] >= healthCheckFailures {
				log.Infof("health check: %s answers again, back in rotation", base)
			}
			failures[auth.ID] = 0
			s.coreManager.SetHealthBlock(auth.ID, time.Time{})
			continue
		}
		failures[auth.ID]++
		if failures[auth.ID] < healthCheckFailures {
			continue
		}
		if failures[auth.ID] == healthCheckFailures {
			log.Warnf("health check: %s failed %d times in a row, out of rotation: %v", base, healthCheckFailures, errProbe)
		}
		s.coreManager.SetHealthBlock(auth.ID, time.Now().Add(2*healthCheckInterval+healthCheckTimeout))
	}
	for id := range failures {
		if !checked[id] {
			delete(failures, id)
		}
	}
}

func (s *Service) probeHealth(ctx context.Context, auth *coreauth.Auth, url string) error {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if errReq != nil {
		return errReq
	}
	resp, errDo := helps.NewProxyAwareHTTPClient(ctx, s.cfg, auth, healthCheckTimeout).Do(req)
	if errDo != nil {
		return errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("health check: close body: %v", errClose)
		}
	}()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}
