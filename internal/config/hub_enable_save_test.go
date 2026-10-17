package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// scripts/hub-enable.sh against configs written by the proxy's own save path
// (SaveConfigPreserveComments, as every management save does), which indents
// by 4 spaces. A hand edited 2 space config, then a save, then another apply
// must stay one api-keys.claude list with one pc-hub group that loads.
func TestHubEnableOnManagementSavedConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hub-enable.sh runs on macOS")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash missing")
	}
	script, errAbs := filepath.Abs(filepath.Join("..", "..", "scripts", "hub-enable.sh"))
	if errAbs != nil {
		t.Fatal(errAbs)
	}
	dir := t.TempDir()
	writeKey := func(name, key string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	key1 := writeKey("key1", "dev.save-test."+strings.Repeat("1", 64))
	key2 := writeKey("key2", "dev.save-test."+strings.Repeat("2", 64))
	enable := func(cfg, keyFile string, args ...string) string {
		t.Helper()
		all := append([]string{script, "--config", cfg, "--key-file", keyFile, "--hub-url", "http://127.0.0.1:9", "--proxy-bin", filepath.Join(dir, "none")}, args...)
		out, err := exec.Command("bash", all...).CombinedOutput()
		if err != nil {
			t.Fatalf("hub-enable %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	save := func(path string) {
		t.Helper()
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("load before save: %v", err)
		}
		if err = SaveConfigPreserveComments(path, cfg); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	checkOne := func(path, wantKey string) {
		t.Helper()
		text := read(path)
		if n := len(regexp.MustCompile(`(?m)^api-keys:`).FindAllString(text, -1)); n != 1 {
			t.Fatalf("%d top-level api-keys, want 1:\n%s", n, text)
		}
		if n := len(regexp.MustCompile(`(?m)^ +claude:`).FindAllString(text, -1)); n != 1 {
			t.Fatalf("%d claude lists, want 1", n)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("config does not load: %v", err)
		}
		if len(cfg.ClaudeKey) != 1 || cfg.ClaudeKey[0].APIKey != wantKey || cfg.ClaudeKey[0].Priority != 100 || !cfg.ClaudeKey[0].HealthCheck {
			t.Fatalf("claude keys = %d, want the one pc-hub entry with the expected key", len(cfg.ClaudeKey))
		}
	}
	const base = "config-version: 8\n# Server\nserver:\n  host: 127.0.0.1\n  port: 8317\naccess:\n  api-keys:\n    - \"client-key-placeholder\"\nrouting:\n  strategy: fill-first\n  session-affinity: true\n  session-affinity-rebind-on: quota\n"
	key1Value := "dev.save-test." + strings.Repeat("1", 64)
	key2Value := "dev.save-test." + strings.Repeat("2", 64)

	// 2 space apply, then a management save turns the file into 4 spaces.
	path := filepath.Join(dir, "two.yaml")
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	enable(path, key1, "--apply")
	save(path)
	if !strings.Contains(read(path), "\n    claude:\n        - name: pc-hub\n") {
		t.Fatalf("the save did not write the 4 space layout this test is about:\n%s", read(path))
	}
	saved := read(path)
	if out := enable(path, key1, "--apply"); !strings.Contains(out, "up to date") || read(path) != saved {
		t.Fatalf("same key after a save: %s", out)
	}
	checkOne(path, key1Value)
	if out := enable(path, key2, "--apply"); !strings.Contains(out, "update pc-hub") {
		t.Fatalf("new key after a save: %s", out)
	}
	if out := enable(path, key2, "--apply"); !strings.Contains(out, "up to date") {
		t.Fatalf("rerun with the new key: %s", out)
	}
	checkOne(path, key2Value)

	// A saved config with no upstream keys yet gets a 4 space block.
	path = filepath.Join(dir, "saved.yaml")
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	save(path)
	if out := enable(path, key1, "--apply"); !strings.Contains(out, "append api-keys.claude") {
		t.Fatalf("apply on a saved config: %s", out)
	}
	if !strings.Contains(read(path), "\napi-keys:\n    claude:\n        - name: pc-hub\n") {
		t.Fatalf("block not at the file's 4 space indent:\n%s", read(path))
	}
	if out := enable(path, key1, "--apply"); !strings.Contains(out, "up to date") {
		t.Fatalf("rerun on a saved config: %s", out)
	}
	checkOne(path, key1Value)
	save(path) // the group survives a save and is still current
	if out := enable(path, key1); !strings.Contains(out, "up to date") {
		t.Fatalf("after another save: %s", out)
	}
	checkOne(path, key1Value)
}
