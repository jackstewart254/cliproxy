package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// scripts/hub-enable.sh after real dashboard saves. A Claude key edit renames
// every api-keys.claude group claude-N, and deleting every key writes
// "claude: []". A rerun must find the hub group by base-url, keep exactly one
// hub credential (no stale copy after a rotation), and fill the empty list.
// internal/config TestHubEnableOnManagementSavedConfig covers plain saves.
func TestHubEnableAfterDashboardClaudeKeyEdits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hub-enable.sh runs on macOS")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash missing")
	}
	script, errAbs := filepath.Abs(filepath.Join("..", "..", "..", "..", "scripts", "hub-enable.sh"))
	if errAbs != nil {
		t.Fatal(errAbs)
	}
	const hubURL = "http://127.0.0.1:9"
	dir := t.TempDir()
	key1Value := "dev.save-test." + strings.Repeat("1", 64)
	key2Value := "dev.save-test." + strings.Repeat("2", 64)
	writeKey := func(name, key string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	key1, key2 := writeKey("key1", key1Value), writeKey("key2", key2Value)
	path := filepath.Join(dir, "config.yaml")
	read := func() string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	// --check exits 1 here (no hub answers on port 9), so the output is the result.
	enable := func(keyFile string, args ...string) string {
		t.Helper()
		all := append([]string{script, "--config", path, "--key-file", keyFile, "--hub-url", hubURL, "--proxy-bin", filepath.Join(dir, "none")}, args...)
		out, err := exec.Command("bash", all...).CombinedOutput()
		if err != nil && args[0] != "--check" {
			t.Fatalf("hub-enable %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	dashboard := func(call func(*Handler, *gin.Context), method, body string) {
		t.Helper()
		cfg, err := config.LoadConfigOptional(path, false)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		h := &Handler{cfg: cfg, configFilePath: path}
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(method, "/v0/management/claude-api-key", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		call(h, ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}
	oneHub := func(wantKey string) {
		t.Helper()
		cfg, err := config.LoadConfig(path)
		if err != nil {
			t.Fatalf("config does not load: %v\n%s", err, read())
		}
		var hubs []config.ClaudeKey
		for _, key := range cfg.ClaudeKey {
			if key.BaseURL == hubURL {
				hubs = append(hubs, key)
			}
		}
		if len(hubs) != 1 || hubs[0].APIKey != wantKey || hubs[0].Priority != 100 || !hubs[0].HealthCheck {
			t.Fatalf("%d hub credentials, want 1 with the expected key, priority 100, health-check", len(hubs))
		}
		if strings.Count(read(), "name: pc-hub") != 1 {
			t.Fatalf("want one group named pc-hub:\n%s", strings.ReplaceAll(read(), wantKey, "<key>"))
		}
	}

	base := "config-version: 8\nrouting:\n  strategy: fill-first\napi-keys:\n  claude:\n    - name: other\n      base-url: \"http://other.invalid\"\n      keys:\n        - api-key: \"other-placeholder\"\n"
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	enable(key1, "--apply")
	oneHub(key1Value)

	// Editing the other group renames pc-hub; the hub is still current.
	dashboard((*Handler).PatchClaudeKey, http.MethodPatch, `{"match":"other-placeholder","value":{"priority":5}}`)
	if strings.Contains(read(), "name: pc-hub") {
		t.Fatalf("the save kept the name pc-hub; this test needs the rename:\n%s", strings.ReplaceAll(read(), key1Value, "<key>"))
	}
	if out := enable(key1, "--check"); !strings.Contains(out, "present and current") {
		t.Fatalf("check after a rename: %s", out)
	}
	if out := enable(key1, "--apply"); !strings.Contains(out, "back to pc-hub") {
		t.Fatalf("same key after a rename: %s", out)
	}
	oneHub(key1Value)

	// A rename, then a key rotation: updated in place, no stale copy.
	dashboard((*Handler).PatchClaudeKey, http.MethodPatch, `{"match":"other-placeholder","value":{"priority":6}}`)
	if out := enable(key2, "--apply"); !strings.Contains(out, "update pc-hub") {
		t.Fatalf("new key after a rename: %s", out)
	}
	oneHub(key2Value)
	if strings.Contains(read(), key1Value) {
		t.Fatal("the rotated key is still in the config")
	}

	// An edit of the hub group itself.
	dashboard((*Handler).PatchClaudeKey, http.MethodPatch, `{"match":"`+key2Value+`","value":{"priority":50}}`)
	if out := enable(key2, "--apply"); !strings.Contains(out, "update pc-hub") {
		t.Fatalf("after a hub edit: %s", out)
	}
	oneHub(key2Value)
	if out := enable(key2, "--apply"); !strings.Contains(out, "up to date") {
		t.Fatalf("rerun: %s", out)
	}

	// Every Claude key deleted on the dashboard.
	dashboard((*Handler).PutClaudeKeys, http.MethodPut, `[]`)
	if !strings.Contains(read(), "claude: []") {
		t.Fatalf("the save did not write claude: [], which this test is about:\n%s", read())
	}
	if out := enable(key2, "--apply"); !strings.Contains(out, "fill the empty api-keys.claude") {
		t.Fatalf("apply on claude: []: %s", out)
	}
	oneHub(key2Value)
	if out := enable(key2, "--apply"); !strings.Contains(out, "up to date") {
		t.Fatalf("rerun after filling: %s", out)
	}
}
