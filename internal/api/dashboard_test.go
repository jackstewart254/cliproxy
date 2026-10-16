package api

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/sysstats"
)

func TestFromLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:5000":     true,
		"[::1]:5000":         true,
		"100.65.36.120:5000": false,
		"172.28.96.1:5000":   false,
		"192.168.1.20:5000":  false,
		"not-an-address":     false,
	}
	for addr, want := range cases {
		if got := fromLoopback(addr); got != want {
			t.Errorf("fromLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestPostDashboardSessions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{cfg: &config.Config{}}
	server.managementRoutesEnabled.Store(true)
	post := func(remoteAddr, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/dashboard/sessions", strings.NewReader(body))
		ctx.Request.RemoteAddr = remoteAddr
		server.postDashboardSessions(ctx)
		return rec
	}

	body := `{"machine":"mbp-m3","sessions":[{"id":"post-test-1","title":"One"},{"id":"post-test-2","title":"Two"},{"id":""}]}`
	if rec := post("192.168.1.20:5000", body); rec.Code != http.StatusForbidden {
		t.Fatalf("LAN caller: status %d, want 403", rec.Code)
	}
	rec := post("127.0.0.1:5000", body)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"updated":2}` {
		t.Fatalf("loopback caller: status %d body %s, want 200 and 2 updated", rec.Code, rec.Body.String())
	}
	if rec := post("127.0.0.1:5000", `{"sessions":`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status %d, want 400", rec.Code)
	}
	huge := `{"machine":"m","sessions":[{"id":"x","title":"` + strings.Repeat("a", maxDashboardSessionsBody) + `"}]}`
	if rec := post("127.0.0.1:5000", huge); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over 1 MB: status %d, want 413", rec.Code)
	}
}

// The data reply is gzipped for a browser that asks, and plain otherwise,
// with the same JSON either way.
func TestServeDashboardDataGzip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{cfg: &config.Config{}, mgmt: &managementHandlers.Handler{}}
	server.managementRoutesEnabled.Store(true)
	get := func(encoding string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/dashboard/data?range=24h", nil)
		ctx.Request.RemoteAddr = "127.0.0.1:5000"
		if encoding != "" {
			ctx.Request.Header.Set("Accept-Encoding", encoding)
		}
		server.serveDashboardData(ctx)
		return rec
	}
	plain := get("")
	if plain.Code != http.StatusOK || plain.Header().Get("Content-Encoding") != "" {
		t.Fatalf("plain: status %d encoding %q", plain.Code, plain.Header().Get("Content-Encoding"))
	}
	zipped := get("br, gzip;q=0.8")
	if zipped.Code != http.StatusOK || zipped.Header().Get("Content-Encoding") != "gzip" || zipped.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("gzip: status %d encoding %q vary %q", zipped.Code, zipped.Header().Get("Content-Encoding"), zipped.Header().Get("Vary"))
	}
	zr, errReader := gzip.NewReader(zipped.Body)
	if errReader != nil {
		t.Fatal(errReader)
	}
	body, errRead := io.ReadAll(zr)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var a, b map[string]any
	if json.Unmarshal(body, &a) != nil || json.Unmarshal(plain.Body.Bytes(), &b) != nil || a["summary"] == nil || b["summary"] == nil {
		t.Fatal("replies are not the dashboard JSON")
	}
	if refused := get("gzip;q=0"); refused.Header().Get("Content-Encoding") != "" {
		t.Fatal("gzip sent although the browser refused it")
	}
	for header, want := range map[string]bool{"gzip": true, "deflate, gzip": true, "GZIP; q=1": true, "gzip; q=0": false, "identity": false, "": false} {
		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}

func TestServeDashboardApp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{cfg: &config.Config{}}
	server.managementRoutesEnabled.Store(true)
	engine := gin.New()
	engine.GET("/dashboard", server.serveDashboard)
	engine.GET("/dashboard/static/*filepath", server.serveDashboardStatic)
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	page := get("/dashboard")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `src="/dashboard/static/app.js"`) {
		t.Fatalf("page: status %d, want 200 and a script tag for app.js", page.Code)
	}
	for _, path := range []string{"app.js", "core.js", "app.css", "screens/overview.js", "screens/routing.js"} {
		rec := get("/dashboard/static/" + path)
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Fatalf("%s: status %d, %d bytes", path, rec.Code, rec.Body.Len())
		}
		if strings.HasSuffix(path, ".js") && !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
			t.Fatalf("%s: content type %q, want javascript so the browser runs it as a module", path, rec.Header().Get("Content-Type"))
		}
	}
	if rec := get("/dashboard/static/missing.js"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing file: status %d, want 404", rec.Code)
	}

	server.managementRoutesEnabled.Store(false)
	if rec := get("/dashboard/static/app.js"); rec.Code != http.StatusNotFound {
		t.Fatalf("dashboard off: status %d, want 404", rec.Code)
	}
}

// The Devices tab reads this host, and each configured device through that
// device's own management route with its key. A device that is down or
// refuses the key shows offline with the reason. A 403 (a ban) is asked again
// on the next poll; a 401 (wrong key) is not, until the config reloads.
func TestServeDashboardDevices(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("remote-management: {secret-key: pc-key}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	pcCfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	pc := &Server{cfg: pcCfg, engine: gin.New(), mgmt: managementHandlers.NewHandler(pcCfg, path, nil)}
	pc.managementRoutesEnabled.Store(true)
	pc.registerManagementRoutes()
	var banned atomic.Bool
	var wrongTries atomic.Int32
	banned.Store(true)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Management-Key") == "nope" {
			wrongTries.Add(1)
		} else if banned.CompareAndSwap(true, false) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"IP banned due to too many failed attempts. Try again in 30m0s"}`)
			return
		}
		pc.engine.ServeHTTP(w, r)
	}))
	defer remote.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	cfg := &config.Config{}
	cfg.RemoteManagement.Devices = []config.Device{
		{Name: "pc", URL: remote.URL, ManagementKey: "pc-key"},
		{Name: "wrong-key", URL: remote.URL, ManagementKey: "nope"},
		{Name: "down", URL: gone.URL, ManagementKey: "pc-key"},
	}
	mac := &Server{cfg: cfg, mgmt: managementHandlers.NewHandler(cfg, "", nil)}
	mac.managementRoutesEnabled.Store(true)
	type device struct {
		ID     string
		Online bool
		Error  string
		Stats  *sysstats.Stats
	}
	read := func() []device {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/dashboard/devices", nil)
		ctx.Request.RemoteAddr = "127.0.0.1:5000"
		mac.serveDashboardDevices(ctx)
		var reply struct{ Devices []device }
		if errJSON := json.Unmarshal(rec.Body.Bytes(), &reply); errJSON != nil || rec.Code != http.StatusOK || len(reply.Devices) != 4 {
			t.Fatalf("status %d, %v, body %s", rec.Code, errJSON, rec.Body.String())
		}
		return reply.Devices
	}

	first := read()
	local, st := first[0], first[0].Stats
	if local.ID != "local" || !local.Online || st == nil || st.CPU.Cores == 0 || len(st.CPU.PerCore) != st.CPU.Cores ||
		st.Memory.Total == 0 || st.Headroom.MemoryAvailable == 0 || st.Processes == 0 || len(st.TopCPU) == 0 || len(st.TopMemory) == 0 || st.TopMemory[0].RSS == 0 {
		t.Fatalf("local reading incomplete: %+v", local)
	}
	if pcDev := first[1]; pcDev.ID != "1" || pcDev.Online || !strings.Contains(pcDev.Error, "IP banned") {
		t.Fatalf("banned pc: %+v, want offline with the ban", pcDev)
	}
	if wrong := first[2]; wrong.Online || !strings.Contains(wrong.Error, "invalid management key") {
		t.Fatalf("wrong key: %+v, want offline with the refusal", wrong)
	}
	if down := first[3]; down.Online || !strings.Contains(down.Error, "unreachable") {
		t.Fatalf("down: %+v, want offline and unreachable", down)
	}

	second := read()
	if pcDev := second[1]; !pcDev.Online || pcDev.Stats == nil || pcDev.Stats.Hostname != st.Hostname {
		t.Fatalf("pc after the ban: %+v, want its reading fetched with its key", pcDev)
	}
	if wrong := second[2]; wrong.Online || !strings.Contains(wrong.Error, "refused") || wrongTries.Load() != 1 {
		t.Fatalf("wrong key again: %+v after %d tries, want it not asked again", wrong, wrongTries.Load())
	}
	mac.mgmt.SetConfig(cfg)
	read()
	if wrongTries.Load() != 2 {
		t.Fatalf("after a config reload the wrong key was tried %d times in all, want 2", wrongTries.Load())
	}
}
