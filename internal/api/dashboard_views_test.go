package api

import (
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// testViewsBody is a views document as the server stores it after the first
// save. The revision a client sends is ignored, so a PUT with If-Match "0"
// stores and returns exactly this.
const testViewsBody = `{"views":[{"id":"spend","name":"Spend","panels":[{"type":"tokens","options":{"format":"lines"}}],"columns":["tokens","cost"],"window":"last24h","accounts":null,"builtin":false}],"default":"spend","deleted":[],"revision":1}`

func newViewsTestServer(t *testing.T) (*Server, *gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "dashboard-views.json")
	server := &Server{cfg: &config.Config{}, viewsPathOverride: path}
	server.managementRoutesEnabled.Store(true)
	engine := gin.New()
	engine.GET("/dashboard/views", server.getDashboardViews)
	engine.PUT("/dashboard/views", server.putDashboardViews)
	return server, engine, path
}

func viewsRequest(engine *gin.Engine, method, remoteAddr, host, origin, body string) *httptest.ResponseRecorder {
	return viewsRequestIfMatch(engine, method, remoteAddr, host, origin, "", body)
}

// viewsRequestIfMatch is viewsRequest with an If-Match header, left out when
// ifMatch is empty.
func viewsRequestIfMatch(engine *gin.Engine, method, remoteAddr, host, origin, ifMatch, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/dashboard/views", strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	engine.ServeHTTP(rec, req)
	return rec
}

// revisionPattern finds a document's revision field.
var revisionPattern = regexp.MustCompile(`"revision":-?[0-9]+`)

// withRevision is doc with its revision set to revision.
func withRevision(doc string, revision int64) string {
	return revisionPattern.ReplaceAllString(doc, `"revision":`+strconv.FormatInt(revision, 10))
}

// ifMatch is the If-Match header value for revision.
func ifMatch(revision int64) string {
	return `"` + strconv.FormatInt(revision, 10) + `"`
}

// baseRevision is the revision an empty test store hands out.
func baseRevision(t *testing.T, engine *gin.Engine) int64 {
	t.Helper()
	return viewsClient{t: t, engine: engine}.load()
}

func TestDashboardViewsRoundTrip(t *testing.T) {
	_, engine, path := newViewsTestServer(t)
	host := "localhost:8317"

	empty := viewsRequest(engine, http.MethodGet, "127.0.0.1:5000", host, "", "")
	base := viewsClient{t: t, engine: engine}.revisionOf(empty)
	if empty.Code != http.StatusOK || strings.TrimSpace(empty.Body.String()) != withRevision(`{"views":[],"default":"","deleted":[],"revision":0}`, base) || base <= 0 {
		t.Fatalf("empty GET: status %d body %s", empty.Code, empty.Body.String())
	}

	stored := withRevision(testViewsBody, base+1)
	put := viewsRequestIfMatch(engine, http.MethodPut, "127.0.0.1:5000", host, "http://"+host, ifMatch(base), testViewsBody)
	if put.Code != http.StatusOK || strings.TrimSpace(put.Body.String()) != stored {
		t.Fatalf("same-origin PUT: status %d body %s, want 200 and the stored body", put.Code, put.Body.String())
	}
	got := viewsRequest(engine, http.MethodGet, "127.0.0.1:5000", host, "", "")
	if got.Code != http.StatusOK || strings.TrimSpace(got.Body.String()) != stored {
		t.Fatalf("GET after PUT: status %d body %s", got.Code, got.Body.String())
	}
	info, errStat := os.Stat(path)
	if errStat != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("views file %v mode %v, want 0600", errStat, info.Mode().Perm())
	}

	// A script without an Origin header may replace the set.
	replace := withRevision(`{"views":[],"default":"","deleted":[],"revision":0}`, base+2)
	if rec := viewsRequestIfMatch(engine, http.MethodPut, "127.0.0.1:5000", host, "", ifMatch(base+1), replace); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != replace {
		t.Fatalf("PUT without Origin: status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestDashboardViewsRefusals(t *testing.T) {
	server, engine, path := newViewsTestServer(t)
	host := "127.0.0.1:8317"
	base := baseRevision(t, engine)

	for _, tc := range []struct {
		name, method, remote, origin, body string
		want                               int
	}{
		{"LAN GET", http.MethodGet, "192.168.1.20:5000", "", "", http.StatusForbidden},
		{"LAN PUT", http.MethodPut, "192.168.1.20:5000", "", testViewsBody, http.StatusForbidden},
		{"cross-origin PUT", http.MethodPut, "127.0.0.1:5000", "https://evil.example", testViewsBody, http.StatusForbidden},
		{"other port PUT", http.MethodPut, "127.0.0.1:5000", "http://127.0.0.1:9999", testViewsBody, http.StatusForbidden},
		{"null origin PUT", http.MethodPut, "127.0.0.1:5000", "null", testViewsBody, http.StatusForbidden},
		{"file origin PUT", http.MethodPut, "127.0.0.1:5000", "file://", testViewsBody, http.StatusForbidden},
		{"invalid body", http.MethodPut, "127.0.0.1:5000", "http://" + host, `{"views":[{"id":"Bad"}],"default":""}`, http.StatusBadRequest},
		{"unknown field", http.MethodPut, "127.0.0.1:5000", "http://" + host, `{"views":[],"default":"","x":1}`, http.StatusBadRequest},
		{"too large", http.MethodPut, "127.0.0.1:5000", "", `{"views":[],"default":"` + strings.Repeat("a", 70<<10) + `"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := viewsRequestIfMatch(engine, tc.method, tc.remote, host, tc.origin, ifMatch(base), tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status %d body %s, want %d", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatal("a refused PUT wrote the views file")
	}

	if rec := viewsRequestIfMatch(engine, http.MethodPut, "127.0.0.1:5000", host, "HTTP://127.0.0.1:8317", ifMatch(base), testViewsBody); rec.Code != http.StatusOK {
		t.Fatalf("same origin in capitals: status %d", rec.Code)
	}

	server.managementRoutesEnabled.Store(false)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		if rec := viewsRequest(engine, method, "127.0.0.1:5000", host, "", testViewsBody); rec.Code != http.StatusNotFound {
			t.Fatalf("dashboard off %s: status %d, want 404", method, rec.Code)
		}
	}
}

// A PUT goes through only when the Origin has the same scheme, hostname and
// effective port as the request. Over TLS the request scheme is https.
func TestDashboardViewsOriginMatchesSchemeHostAndPort(t *testing.T) {
	server, engine, _ := newViewsTestServer(t)
	// revision tracks the stored revision, so every PUT that passes the
	// origin check also passes the revision check.
	revision := baseRevision(t, engine)
	put := func(host, origin string, overTLS bool) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/dashboard/views", strings.NewReader(testViewsBody))
		req.RemoteAddr = "127.0.0.1:5000"
		req.Host = host
		req.Header.Set("Origin", origin)
		req.Header.Set("If-Match", strconv.FormatInt(revision, 10))
		if overTLS {
			req.TLS = &tls.ConnectionState{HandshakeComplete: true}
		}
		engine.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			revision++
		}
		return rec.Code
	}
	for _, tc := range []struct {
		name, host, origin string
		overTLS            bool
		want               int
	}{
		{"same http origin", "dash.example:8317", "http://dash.example:8317", false, http.StatusOK},
		{"https page, http server", "dash.example:8317", "https://dash.example:8317", false, http.StatusForbidden},
		{"http page, https server", "dash.example", "http://dash.example", true, http.StatusForbidden},
		{"same https origin", "dash.example", "https://dash.example", true, http.StatusOK},
		{"https explicit 443 origin", "dash.example", "https://dash.example:443", true, http.StatusOK},
		{"https explicit 443 host", "dash.example:443", "https://dash.example", true, http.StatusOK},
		{"https other port", "dash.example", "https://dash.example:8443", true, http.StatusForbidden},
		{"http explicit 80 origin", "dash.example", "http://dash.example:80", false, http.StatusOK},
		{"http explicit 80 host", "dash.example:80", "http://dash.example", false, http.StatusOK},
		{"http 443 is not default", "dash.example", "http://dash.example:443", false, http.StatusForbidden},
		{"other port", "127.0.0.1:8317", "http://127.0.0.1:9999", false, http.StatusForbidden},
		{"other host", "127.0.0.1:8317", "http://localhost:8317", false, http.StatusForbidden},
		{"host case", "Dash.Example:8317", "http://dash.example:8317", false, http.StatusOK},
		{"ipv6 same", "[::1]:8317", "http://[::1]:8317", false, http.StatusOK},
		{"ipv6 default port", "[2001:db8::1]", "http://[2001:DB8::1]:80", false, http.StatusOK},
		{"ipv6 other port", "[::1]:8317", "http://[::1]:9999", false, http.StatusForbidden},
		{"ipv6 other address", "[::1]:8317", "http://[::2]:8317", false, http.StatusForbidden},
		{"ipv6 without brackets", "[::1]:8317", "http://::1:8317", false, http.StatusForbidden},
		{"null origin", "127.0.0.1:8317", "null", false, http.StatusForbidden},
		{"file origin", "127.0.0.1:8317", "file://", false, http.StatusForbidden},
		{"origin with path", "127.0.0.1:8317", "http://127.0.0.1:8317/dashboard", false, http.StatusForbidden},
		{"origin with user", "127.0.0.1:8317", "http://me@127.0.0.1:8317", false, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := put(tc.host, tc.origin, tc.overTLS); got != tc.want {
				t.Fatalf("Host %q Origin %q TLS %v: status %d, want %d", tc.host, tc.origin, tc.overTLS, got, tc.want)
			}
		})
	}

	// On a TLS listener, a connection the multiplexer hands over wrapped
	// (r.TLS nil) still counts as https.
	server.listenerTLS.Store(true)
	if got := put("dash.example", "https://dash.example", false); got != http.StatusOK {
		t.Fatalf("TLS server, wrapped connection, https origin: status %d, want 200", got)
	}
	if got := put("dash.example", "http://dash.example", false); got != http.StatusForbidden {
		t.Fatalf("TLS server, http origin: status %d, want 403", got)
	}

	// A plain listener whose http.Server has a TLSConfig, as Serve leaves it
	// after setting up HTTP/2, is still http: the live dashboard's own saves
	// were refused when this read the TLSConfig.
	server.listenerTLS.Store(false)
	server.server = &http.Server{TLSConfig: &tls.Config{}}
	if got := put("dash.example:8317", "http://dash.example:8317", false); got != http.StatusOK {
		t.Fatalf("plain server with a TLSConfig, http origin: status %d, want 200", got)
	}
	if got := put("dash.example:8317", "https://dash.example:8317", false); got != http.StatusForbidden {
		t.Fatalf("plain server with a TLSConfig, https origin: status %d, want 403", got)
	}
}

// The largest body the server accepts reads back after it is saved.
func TestDashboardViewsLargestBodyReadsBack(t *testing.T) {
	_, engine, _ := newViewsTestServer(t)
	host := "127.0.0.1:8317"
	var views []string
	for i := 0; i < 50; i++ {
		var panels []string
		for _, panelType := range []string{"allowance", "available", "tokens", "cost", "requests", "output", "cache", "ttft", "latency", "throughput", "failures", "activity"} {
			panels = append(panels, `{"type":"`+panelType+`","options":{"format":"bars","mode":"weekly"}}`)
		}
		id := strings.Repeat("v", 38) + string(rune('a'+i/26)) + string(rune('a'+i%26))
		views = append(views, `{"id":"`+id+`","name":"`+strings.Repeat("é", 60)+`","panels":[`+strings.Join(panels, ",")+
			`],"columns":["account","tokens","requests","input","cache_write","cache_read","output","cost","cache_reuse"],"window":"last7d","accounts":["`+
			strings.Repeat("a", 200)+`"],"builtin":false}`)
	}
	body := `{"views":[` + strings.Join(views, ",") + `],"default":"","deleted":[],"revision":1}`
	if len(body) < 60<<10 || len(body) > 64<<10 {
		t.Fatalf("body is %d bytes, want just under 64 KB", len(body))
	}
	base := baseRevision(t, engine)
	if rec := viewsRequestIfMatch(engine, http.MethodPut, "127.0.0.1:5000", host, "", strconv.FormatInt(base, 10), body); rec.Code != http.StatusOK {
		t.Fatalf("PUT: status %d %s", rec.Code, rec.Body.String())
	}
	got := viewsRequest(engine, http.MethodGet, "127.0.0.1:5000", host, "", "")
	if want := withRevision(body, base+1); got.Code != http.StatusOK || strings.TrimSpace(got.Body.String()) != want {
		t.Fatalf("GET after the largest PUT: status %d, %d bytes, want 200 and the %d bytes stored", got.Code, got.Body.Len(), len(want))
	}
}

func TestDashboardViewsDamagedFile(t *testing.T) {
	_, engine, path := newViewsTestServer(t)
	if errWrite := os.WriteFile(path, []byte("{"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if rec := viewsRequest(engine, http.MethodGet, "127.0.0.1:5000", "127.0.0.1:8317", "", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("damaged file GET: status %d, want 500", rec.Code)
	}
	// A damaged file has no revision to trust. A PUT gets 409 with the empty
	// set at a fresh base revision, and a PUT from that base replaces it.
	probe := viewsRequestIfMatch(engine, http.MethodPut, "127.0.0.1:5000", "127.0.0.1:8317", "", `"0"`, testViewsBody)
	client := viewsClient{t: t, engine: engine}
	base := client.revisionOf(probe)
	if probe.Code != http.StatusConflict || !strings.Contains(probe.Body.String(), `"current":{"views":[],"default":"","deleted":[],"revision":`) || base <= 0 {
		t.Fatalf("PUT over a damaged file from 0: status %d body %s, want 409 with the empty set", probe.Code, probe.Body.String())
	}
	if rec := viewsRequestIfMatch(engine, http.MethodPut, "127.0.0.1:5000", "127.0.0.1:8317", "", ifMatch(base), testViewsBody); rec.Code != http.StatusOK {
		t.Fatalf("PUT over a damaged file from its base: status %d, want 200", rec.Code)
	}
}

func TestDashboardViewsRevisions(t *testing.T) {
	_, engine, path := newViewsTestServer(t)
	host := "127.0.0.1:8317"
	get := func() *httptest.ResponseRecorder {
		return viewsRequest(engine, http.MethodGet, "127.0.0.1:5000", host, "", "")
	}
	put := func(ifMatch, body string) *httptest.ResponseRecorder {
		return viewsRequestIfMatch(engine, http.MethodPut, "127.0.0.1:5000", host, "http://"+host, ifMatch, body)
	}

	client := viewsClient{t: t, engine: engine}

	// An empty store has a base revision, the same on every GET, and the
	// ETag carries it.
	empty := get()
	base := client.revisionOf(empty)
	if empty.Code != http.StatusOK || base <= 0 || empty.Header().Get("ETag") != ifMatch(base) || client.revisionOf(get()) != base {
		t.Fatalf("empty GET: status %d ETag %q body %s", empty.Code, empty.Header().Get("ETag"), empty.Body.String())
	}

	// A file saved before revisions existed gets a base revision too.
	legacy := `{"views":[{"id":"spend","name":"Spend","panels":[],"columns":[],"window":"last24h","accounts":null,"builtin":false}],"default":"spend"}`
	if errWrite := os.WriteFile(path, []byte(legacy), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	legacyGet := get()
	legacyBase := client.revisionOf(legacyGet)
	if legacyGet.Code != http.StatusOK || legacyBase <= base || legacyGet.Header().Get("ETag") != ifMatch(legacyBase) || !strings.Contains(legacyGet.Body.String(), `"default":"spend"`) {
		t.Fatalf("legacy GET: status %d ETag %q body %s, want a base above %d", legacyGet.Code, legacyGet.Header().Get("ETag"), legacyGet.Body.String(), base)
	}

	// The right If-Match moves to the next revision. The body's own revision
	// is ignored.
	first := put(ifMatch(legacyBase), `{"views":[],"default":"spend","revision":99}`)
	r1 := legacyBase + 1
	if first.Code != http.StatusOK || strings.TrimSpace(first.Body.String()) != withRevision(`{"views":[],"default":"spend","deleted":[],"revision":0}`, r1) || first.Header().Get("ETag") != ifMatch(r1) {
		t.Fatalf("PUT If-Match base: status %d ETag %q body %s", first.Code, first.Header().Get("ETag"), first.Body.String())
	}
	// A bare number works too.
	r2 := r1 + 1
	second := put(strconv.FormatInt(r1, 10), `{"views":[],"default":"","revision":1}`)
	if second.Code != http.StatusOK || strings.TrimSpace(second.Body.String()) != withRevision(`{"views":[],"default":"","deleted":[],"revision":0}`, r2) {
		t.Fatalf("PUT If-Match bare: status %d body %s", second.Code, second.Body.String())
	}
	stored, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}

	// A stale or malformed If-Match gets 409 with the current document and
	// writes nothing.
	current := strings.TrimSpace(get().Body.String())
	for _, value := range []string{ifMatch(r1), ifMatch(legacyBase), ifMatch(base), `"0"`, ifMatch(r2 + 1), "abc", `W/` + ifMatch(r2), "*", `"-1"`, `""`} {
		rec := put(value, `{"views":[],"default":"lost"}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("If-Match %s: status %d body %s, want 409", value, rec.Code, rec.Body.String())
		}
		want := `{"current":` + current + `,"error":"views changed since you loaded them","revision":` + strconv.FormatInt(r2, 10) + `}`
		if got := strings.TrimSpace(rec.Body.String()); got != want {
			t.Fatalf("If-Match %s: body %s, want %s", value, got, want)
		}
		if rec.Header().Get("ETag") != ifMatch(r2) {
			t.Fatalf("If-Match %s: ETag %q", value, rec.Header().Get("ETag"))
		}
	}

	// No If-Match gets 428.
	missing := put("", `{"views":[],"default":"lost"}`)
	if missing.Code != http.StatusPreconditionRequired || strings.TrimSpace(missing.Body.String()) != `{"error":"If-Match header required"}` {
		t.Fatalf("PUT without If-Match: status %d body %s, want 428", missing.Code, missing.Body.String())
	}

	if after, _ := os.ReadFile(path); string(after) != string(stored) {
		t.Fatalf("refused PUTs changed the file to %s", after)
	}
}

// Two tabs save from the same revision at once: exactly one wins.
func TestDashboardViewsConcurrentPutsFromOneRevision(t *testing.T) {
	_, engine, _ := newViewsTestServer(t)
	host := "127.0.0.1:8317"
	base := baseRevision(t, engine)
	for round := 0; round < 20; round++ {
		ifMatch := strconv.FormatInt(base+int64(round), 10)
		start := make(chan struct{})
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i := range codes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				body := `{"views":[],"default":"tab-` + strconv.Itoa(i) + `"}`
				codes[i] = viewsRequestIfMatch(engine, http.MethodPut, "127.0.0.1:5000", host, "", ifMatch, body).Code
			}(i)
		}
		close(start)
		wg.Wait()
		ok, conflict := 0, 0
		for _, code := range codes {
			switch code {
			case http.StatusOK:
				ok++
			case http.StatusConflict:
				conflict++
			}
		}
		if ok != 1 || conflict != 1 {
			t.Fatalf("round %d: statuses %v, want one 200 and one 409", round, codes)
		}
	}
	if rec := viewsRequest(engine, http.MethodGet, "127.0.0.1:5000", host, "", ""); (viewsClient{t: t, engine: engine}).revisionOf(rec) != base+20 {
		t.Fatalf("after 20 rounds: %s, want revision %d", rec.Body.String(), base+20)
	}
}

func TestDashboardViewsUpdatedAtAndDeletedRoundTrip(t *testing.T) {
	_, engine, _ := newViewsTestServer(t)
	host := "127.0.0.1:8317"
	body := `{"views":[{"id":"spend","name":"Spend","panels":[{"type":"cost","options":{}}],"columns":[],"window":"last24h","accounts":null,"builtin":false,"updated_at":1760090000123}],` +
		`"default":"spend","deleted":[{"id":"old","updated_at":1760080000000}],"revision":1}`
	base := baseRevision(t, engine)
	want := withRevision(body, base+1)
	if rec := viewsRequestIfMatch(engine, http.MethodPut, "127.0.0.1:5000", host, "", ifMatch(base), body); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("PUT: status %d body %s", rec.Code, rec.Body.String())
	}
	if rec := viewsRequest(engine, http.MethodGet, "127.0.0.1:5000", host, "", ""); strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("GET: %s, want %s", rec.Body.String(), want)
	}
	bad := `{"views":[],"default":"","deleted":[{"id":"Old","updated_at":1}]}`
	if rec := viewsRequestIfMatch(engine, http.MethodPut, "127.0.0.1:5000", host, "", ifMatch(base+1), bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad deleted id: status %d, want 400", rec.Code)
	}
}

// viewsClient drives the views endpoints like a dashboard tab.
type viewsClient struct {
	t      *testing.T
	engine *gin.Engine
}

// revisionOf reads "revision" from a document, or from "current" in a 409.
func (c viewsClient) revisionOf(rec *httptest.ResponseRecorder) int64 {
	c.t.Helper()
	var body struct {
		Revision json.Number `json:"revision"`
	}
	decoder := json.NewDecoder(strings.NewReader(rec.Body.String()))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&body); errDecode != nil {
		c.t.Fatalf("response %q: %v", rec.Body.String(), errDecode)
	}
	revision, errParse := body.Revision.Int64()
	if errParse != nil {
		c.t.Fatalf("response %q: revision %v", rec.Body.String(), errParse)
	}
	return revision
}

func (c viewsClient) get() *httptest.ResponseRecorder {
	return viewsRequest(c.engine, http.MethodGet, "127.0.0.1:5000", "127.0.0.1:8317", "", "")
}

func (c viewsClient) put(revision int64, defaultView string) *httptest.ResponseRecorder {
	return viewsRequestIfMatch(c.engine, http.MethodPut, "127.0.0.1:5000", "127.0.0.1:8317", "",
		`"`+strconv.FormatInt(revision, 10)+`"`, `{"views":[],"default":"`+defaultView+`"}`)
}

// load is the revision a tab starts from: GET's, or, when GET cannot read a
// damaged file, the one a refused PUT reports.
func (c viewsClient) load() int64 {
	c.t.Helper()
	if rec := c.get(); rec.Code == http.StatusOK {
		return c.revisionOf(rec)
	}
	probe := viewsRequestIfMatch(c.engine, http.MethodPut, "127.0.0.1:5000", "127.0.0.1:8317", "", "probe", `{"views":[],"default":""}`)
	if probe.Code != http.StatusConflict {
		c.t.Fatalf("probe PUT: status %d body %s, want 409", probe.Code, probe.Body.String())
	}
	return c.revisionOf(probe)
}

// save saves from revision and returns the new one, failing unless it is 200.
func (c viewsClient) save(revision int64, defaultView string) int64 {
	c.t.Helper()
	rec := c.put(revision, defaultView)
	if rec.Code != http.StatusOK {
		c.t.Fatalf("PUT from revision %d: status %d body %s, want 200", revision, rec.Code, rec.Body.String())
	}
	return c.revisionOf(rec)
}

// When the views file is deleted, damaged or replaced by a file without a
// revision, the store starts again from a revision no tab has seen. A tab
// that loaded before that cannot overwrite the recovered document.
func TestDashboardViewsRecoveryRefusesStaleTabs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(path string) error
	}{
		{"deleted", os.Remove},
		{"damaged", func(path string) error { return os.WriteFile(path, []byte("{"), 0o600) }},
		{"legacy restored", func(path string) error {
			return os.WriteFile(path, []byte(`{"views":[],"default":"restored"}`), 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, engine, path := newViewsTestServer(t)
			client := viewsClient{t: t, engine: engine}
			first := client.save(client.load(), "first")
			staleA := first
			staleB := client.save(first, "second")

			if errDamage := tc.damage(path); errDamage != nil {
				t.Fatal(errDamage)
			}
			base := client.load()
			if base == staleA || base == staleB || base == 0 {
				t.Fatalf("recovery base %d reuses a revision handed out before (%d, %d) or 0", base, staleA, staleB)
			}
			recovered := client.save(base, "recovered")
			if recovered != base+1 {
				t.Fatalf("recovered at %d, want %d", recovered, base+1)
			}
			for _, stale := range []int64{staleA, staleB, 0} {
				if rec := client.put(stale, "stale"); rec.Code != http.StatusConflict {
					t.Fatalf("stale tab at revision %d: status %d body %s, want 409", stale, rec.Code, rec.Body.String())
				}
			}
			if rec := client.get(); client.revisionOf(rec) != recovered || !strings.Contains(rec.Body.String(), `"default":"recovered"`) {
				t.Fatalf("after stale PUTs: %s", rec.Body.String())
			}
		})
	}
}

// An empty store never hands out revision 0, so If-Match "0" matches nothing.
func TestDashboardViewsEmptyStoreIsNotRevisionZero(t *testing.T) {
	_, engine, _ := newViewsTestServer(t)
	client := viewsClient{t: t, engine: engine}
	base := client.revisionOf(client.get())
	if base <= 0 || base > 1<<53-1 {
		t.Fatalf("empty store revision %d, want 1 to 2^53-1", base)
	}
	if again := client.revisionOf(client.get()); again != base {
		t.Fatalf("second GET revision %d, want the same %d", again, base)
	}
	if rec := client.put(0, "zero"); rec.Code != http.StatusConflict {
		t.Fatalf("If-Match 0 on an empty store: status %d, want 409", rec.Code)
	}
	if saved := client.save(base, "first"); saved != base+1 {
		t.Fatalf("saved at %d, want %d", saved, base+1)
	}
}

// A stored revision outside 0 to 2^53-1 makes the file damaged, and a save
// never takes the revision past 2^53-1.
func TestDashboardViewsRevisionLimits(t *testing.T) {
	_, engine, path := newViewsTestServer(t)
	client := viewsClient{t: t, engine: engine}

	for _, stored := range []string{"9223372036854775807", "9007199254740992", "-1"} {
		if errWrite := os.WriteFile(path, []byte(`{"views":[],"default":"","revision":`+stored+`}`), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
		if rec := client.get(); rec.Code != http.StatusInternalServerError {
			t.Fatalf("stored revision %s: GET status %d body %s, want 500", stored, rec.Code, rec.Body.String())
		}
		base := client.load()
		if base <= 0 || base > 1<<53-1 {
			t.Fatalf("stored revision %s: recovery base %d", stored, base)
		}
		if saved := client.save(base, "fixed"); saved != base+1 {
			t.Fatalf("stored revision %s: saved at %d, want %d", stored, saved, base+1)
		}
	}

	top := `{"views":[],"default":"top","deleted":[],"revision":9007199254740991}`
	if errWrite := os.WriteFile(path, []byte(top), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if got := client.revisionOf(client.get()); got != 1<<53-1 {
		t.Fatalf("GET revision %d, want 2^53-1", got)
	}
	if rec := client.put(1<<53-1, "past"); rec.Code == http.StatusOK {
		t.Fatalf("save past 2^53-1 succeeded: %s", rec.Body.String())
	}
	if data, _ := os.ReadFile(path); string(data) != top {
		t.Fatalf("refused save changed the file to %s", data)
	}
}

// A plain connection through the protocol multiplexer is http. Its
// bufferedConn reports an empty TLS state, which net/http turns into a
// non-nil r.TLS, and the live dashboard's own saves were refused as
// cross-origin when that counted as https.
func TestDashboardViewsPlainMuxConnectionIsHTTP(t *testing.T) {
	server, engine, _ := newViewsTestServer(t)
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen: %v", errListen)
	}
	muxLn := newMuxListener(listener.Addr(), 1024)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		_ = server.acceptMuxConnections(listener, muxLn)
	}()
	srv := &http.Server{Handler: engine}
	server.server = srv
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = srv.Serve(muxLn)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = listener.Close()
		<-serveDone
		<-acceptDone
	})

	addr := listener.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}
	put := func(origin string) int {
		req, errReq := http.NewRequest(http.MethodPut, "http://"+addr+"/dashboard/views", strings.NewReader("{"))
		if errReq != nil {
			t.Fatalf("request: %v", errReq)
		}
		req.Header.Set("Origin", origin)
		req.Header.Set("If-Match", `"1"`)
		resp, errDo := client.Do(req)
		if errDo != nil {
			t.Fatalf("PUT: %v", errDo)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}
	// The body is not JSON, so a request past the origin check gets 400.
	if got := put("http://" + addr); got != http.StatusBadRequest {
		t.Fatalf("plain connection, http origin: status %d, want 400 (past the origin check)", got)
	}
	if got := put("https://" + addr); got != http.StatusForbidden {
		t.Fatalf("plain connection, https origin: status %d, want 403", got)
	}
}
