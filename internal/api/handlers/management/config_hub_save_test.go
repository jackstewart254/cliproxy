package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

const hubV8Config = `config-version: 8
api-keys:
  claude:
    - name: hub
      base-url: "http://hub.example"
      priority: 100
      request-scoped-errors:
        - status: 0
          match: ["dial tcp"]
          action: "continue-and-cooldown"
      keys:
        - api-key: "hub-key"
          health-check: true
`

// The hub key without rules, for adding one through the API.
const hubV8ConfigNoRules = `config-version: 8
api-keys:
  claude:
    - name: hub
      base-url: "http://hub.example"
      keys:
        - api-key: "hub-key"
          health-check: true
`

// Saves through the management API keep an explicit `status: 0` (not pruned as
// a zero value) and keep health-check on the key entry, where an older binary
// ignores it, instead of regrouping it to the group, where it is rejected.
func TestManagementSavesKeepHubFields(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		call    func(*Handler, *gin.Context)
		method  string
		body    string
		apiKey  string
		healthy bool
	}{
		{name: "edit key", base: hubV8Config, call: (*Handler).PatchClaudeKey, method: http.MethodPatch, apiKey: "hub-key", healthy: true,
			body: `{"index":0,"value":{"priority":50}}`},
		{name: "add rule", base: hubV8ConfigNoRules, call: (*Handler).PatchClaudeKey, method: http.MethodPatch, apiKey: "hub-key", healthy: true,
			body: `{"index":0,"value":{"request-scoped-errors":[{"status":0,"match":["EOF"],"action":"continue-and-cooldown"}]}}`},
		{name: "add key", base: "config-version: 8\nrouting:\n  strategy: fill-first\n", call: (*Handler).PutClaudeKeys, method: http.MethodPut, apiKey: "new-key", healthy: true,
			body: `[{"api-key":"hub-key","base-url":"http://hub.example","health-check":true,"request-scoped-errors":[{"status":0,"match":["dial tcp"],"action":"continue-and-cooldown"}]},` +
				`{"api-key":"new-key","base-url":"http://new.example","health-check":true,"request-scoped-errors":[{"status":0,"match":["EOF"],"action":"continue-and-cooldown"}]}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.base), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadConfigOptional(path, false)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			h := &Handler{cfg: cfg, configFilePath: path}
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(tc.method, "/v0/management/claude-api-key", strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			tc.call(h, ctx)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}

			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				APIKeys struct {
					Claude []map[string]any `yaml:"claude"`
				} `yaml:"api-keys"`
			}
			if err = yaml.Unmarshal(saved, &doc); err != nil {
				t.Fatalf("saved file: %v\n%s", err, saved)
			}
			for _, group := range doc.APIKeys.Claude {
				if _, ok := group["health-check"]; ok {
					t.Fatalf("health-check regrouped to group level:\n%s", saved)
				}
			}
			reloaded, err := config.LoadConfigOptional(path, false)
			if err != nil {
				t.Fatalf("reload saved file: %v\n%s", err, saved)
			}
			found := false
			for _, key := range reloaded.ClaudeKey {
				if key.APIKey != tc.apiKey {
					continue
				}
				found = true
				if key.HealthCheck != tc.healthy {
					t.Fatalf("health-check = %v after save:\n%s", key.HealthCheck, saved)
				}
				if len(key.RequestScopedErrors) == 0 || key.RequestScopedErrors[0].Status != config.StatusNoHTTP {
					t.Fatalf("explicit status 0 lost after save: %+v\n%s", key.RequestScopedErrors, saved)
				}
			}
			if !found {
				t.Fatalf("key %s missing after save:\n%s", tc.apiKey, saved)
			}
		})
	}
}

// The same holds for rules under oauth-request-scoped-errors.<provider>.
func TestManagementSavesKeepOAuthRuleStatusZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("config-version: 8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/oauth-request-scoped-errors",
		strings.NewReader(`{"claude":[{"status":0,"match":["EOF"],"action":"continue-and-cooldown"}]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PutOAuthRequestScopedErrors(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("reload saved file: %v\n%s", err, saved)
	}
	if rules := reloaded.OAuthRequestScopedErrors["claude"]; len(rules) == 0 || rules[0].Status != config.StatusNoHTTP {
		t.Fatalf("explicit status 0 lost after save: %+v\n%s", rules, saved)
	}
}
