package api

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
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
