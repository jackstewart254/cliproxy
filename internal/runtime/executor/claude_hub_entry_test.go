package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// The pc-hub group that scripts/hub-enable.sh installs loads as a priority 100,
// health-checked credential with the status 0 rule, and hands the hub each
// caller's Claude Code session id, also for Desktop sessions the proxy does
// not confirm as native Claude Code. The hub keys session affinity on it.
func TestHubEntryForwardsSessionID(t *testing.T) {
	const sessionID = "11111111-2222-4333-8444-555555555555"
	const hubKey = "dev.test-mac.0123456789abcdef0123456789abcdef"
	type seen struct {
		header, bodySession, auth string
	}
	got := make(chan seen, 1)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- seen{
			header:      r.Header.Get("X-Claude-Code-Session-Id"),
			bodySession: gjson.Get(gjson.GetBytes(body, "metadata.user_id").String(), "session_id").String(),
			auth:        r.Header.Get("Authorization"),
		}
		if r.Header.Get("Accept") == "text/event-stream" || gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer hub.Close()

	template, errRead := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "hub-entry.yaml"))
	if errRead != nil {
		t.Fatalf("read hub entry template: %v", errRead)
	}
	var group strings.Builder
	for _, line := range strings.Split(string(template), "\n") {
		if !strings.HasPrefix(line, "#") {
			group.WriteString(strings.NewReplacer("__HUB_URL__", hub.URL, "__HUB_KEY__", hubKey).Replace(line) + "\n")
		}
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte("config-version: 8\napi-keys:\n  claude:\n"+group.String()), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := config.LoadConfig(path)
	if errLoad != nil {
		t.Fatalf("load config with the hub entry: %v", errLoad)
	}
	if len(cfg.ClaudeKey) != 1 {
		t.Fatalf("claude keys = %d, want 1", len(cfg.ClaudeKey))
	}
	if entry := cfg.ClaudeKey[0]; entry.Priority != 100 || !entry.HealthCheck || len(entry.RequestScopedErrors) != 1 || entry.RequestScopedErrors[0].Status != config.StatusNoHTTP {
		t.Fatalf("hub entry = priority %d health-check %v rules %+v, want 100, true and one status 0 rule", entry.Priority, entry.HealthCheck, entry.RequestScopedErrors)
	}
	auths, errSynth := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator()})
	if errSynth != nil || len(auths) != 1 {
		t.Fatalf("synthesize = %d auths, %v; want the hub", len(auths), errSynth)
	}
	auth := auths[0]
	if auth.Attributes["priority"] != "100" || auth.Attributes["health_check"] != "true" {
		t.Fatalf("hub auth attributes priority %q health_check %q", auth.Attributes["priority"], auth.Attributes["health_check"])
	}

	userID := `{"device_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","account_uuid":"","session_id":"` + sessionID + `"}`
	executor := NewClaudeExecutor(cfg)
	for _, tc := range []struct {
		name      string
		userAgent string
		stream    bool
	}{
		{name: "desktop", userAgent: "claude-cli/2.1.299 (external, claude-desktop)"},
		{name: "desktop stream", userAgent: "claude-cli/2.1.299 (external, claude-desktop)", stream: true},
		{name: "cli", userAgent: "claude-cli/2.1.280 (external, cli)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":8,"stream":%t,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":%q}}`, tc.stream, userID))
			incoming := http.Header{
				"User-Agent":               {tc.userAgent},
				"X-App":                    {"cli"},
				"Anthropic-Beta":           {"claude-code-20250219"},
				"X-Claude-Code-Session-Id": {sessionID},
			}
			req := cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: payload}
			opts := cliproxyexecutor.Options{Stream: tc.stream, SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload, Headers: incoming}
			if tc.stream {
				result, errStream := executor.ExecuteStream(context.Background(), auth, req, opts)
				if errStream != nil {
					t.Fatalf("ExecuteStream: %v", errStream)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream chunk: %v", chunk.Err)
					}
				}
			} else if _, errExecute := executor.Execute(context.Background(), auth, req, opts); errExecute != nil {
				t.Fatalf("Execute: %v", errExecute)
			}
			s := <-got
			if s.header != sessionID || s.bodySession != sessionID {
				t.Fatalf("hub got session header %q and body session %q, want %q for both", s.header, s.bodySession, sessionID)
			}
			if s.auth != "Bearer "+hubKey {
				t.Fatal("hub did not get the device key as a bearer token")
			}
		})
	}
}
