package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	log "github.com/sirupsen/logrus"
)

// dashboardViewsPath is where saved dashboard views live: next to the usage
// stats file, or the test override.
func (s *Server) dashboardViewsPath() string {
	if s.viewsPathOverride != "" {
		return s.viewsPathOverride
	}
	return usagestats.Default().ViewsPath()
}

// servesTLS reports whether Start put this server's listener behind TLS. It
// reads what Start recorded, not the config, which can change on reload
// without a restart, and not the http.Server's TLSConfig, which Serve fills
// in on a plain listener too.
func (s *Server) servesTLS() bool {
	return s.listenerTLS.Load()
}

// sameOrigin reports whether a browser request comes from a page served by
// this server. A request without an Origin header (a script or curl) passes.
// Otherwise the Origin must be a bare http or https origin whose scheme,
// hostname and effective port (80 or 443 when left out) equal the request's.
// The request is https when its TLS handshake completed or when serverTLS
// says the listener is TLS: the multiplexer hands a TLS connection without
// ALPN to net/http wrapped, which can leave r.TLS nil. A bare r.TLS proves
// nothing, because the multiplexer's bufferedConn reports an empty TLS state
// for plain connections too. X-Forwarded-Proto is not used,
// because nothing in this server trusts it. An Origin of "null" (sandboxed
// or file pages) fails. The server's CORS middleware allows every origin, so
// this is what stops a page on another site, open in a browser on this
// host, from changing the views.
func sameOrigin(r *http.Request, serverTLS bool) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, errParse := url.Parse(origin)
	if errParse != nil || parsed.User != nil || parsed.Opaque != "" || (parsed.Path != "" && parsed.Path != "/") ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Hostname() == "" {
		return false
	}
	scheme := "http"
	if (r.TLS != nil && r.TLS.HandshakeComplete) || serverTLS {
		scheme = "https"
	}
	if !strings.EqualFold(parsed.Scheme, scheme) {
		return false
	}
	requestHost := &url.URL{Host: r.Host}
	if requestHost.Hostname() == "" {
		return false
	}
	return strings.EqualFold(parsed.Hostname(), requestHost.Hostname()) &&
		effectivePort(parsed.Port(), scheme) == effectivePort(requestHost.Port(), scheme)
}

// effectivePort is port, or the scheme's default port when port is empty.
func effectivePort(port, scheme string) string {
	if port != "" {
		return port
	}
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// dashboardViewsAllowed applies the read-only dashboard's access rule to the
// views endpoints: the dashboard must be on and the caller on this host. It
// writes the refusal and reports whether to go on.
func (s *Server) dashboardViewsAllowed(c *gin.Context) bool {
	if !s.dashboardAvailable() {
		c.AbortWithStatus(http.StatusNotFound)
		return false
	}
	if !fromLoopback(c.Request.RemoteAddr) {
		c.AbortWithStatus(http.StatusForbidden)
		return false
	}
	c.Header("Cache-Control", "no-store")
	return true
}

// viewsETag is the entity tag of a views revision: the number in quotes.
func viewsETag(revision int64) string {
	return `"` + strconv.FormatInt(revision, 10) + `"`
}

// parseViewsIfMatch reads an If-Match value as a views revision: a number in
// quotes, such as "3", or the bare number. Anything else is not a revision.
func parseViewsIfMatch(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	if value == "" || strings.TrimLeft(value, "0123456789") != "" {
		return 0, false
	}
	revision, errParse := strconv.ParseInt(value, 10, 64)
	if errParse != nil {
		return 0, false
	}
	return revision, true
}

// getDashboardViews returns the saved dashboard views:
// {"views": [...], "default": "<id or empty>", "deleted": [...],
// "revision": N}, with the revision also as the ETag header.
func (s *Server) getDashboardViews(c *gin.Context) {
	if !s.dashboardViewsAllowed(c) {
		return
	}
	path := s.dashboardViewsPath()
	if path == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "saved views need a usage stats file"})
		return
	}
	views, errRead := usagestats.ReadViews(path)
	if errRead != nil {
		log.Warnf("dashboard: read saved views: %v", errRead)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "saved views file cannot be read"})
		return
	}
	c.Header("ETag", viewsETag(views.Revision))
	c.JSON(http.StatusOK, views)
}

// putDashboardViews replaces the saved dashboard views with the request body,
// after usagestats.DecodeViews checks it, when the If-Match header names the
// stored revision. Without If-Match it answers 428. When the revision moved
// on, or If-Match is not a revision, it answers 409 with the current
// revision and document. Besides the read-only dashboard's access rule it
// refuses a cross-origin browser request.
func (s *Server) putDashboardViews(c *gin.Context) {
	if !s.dashboardViewsAllowed(c) {
		return
	}
	if !sameOrigin(c.Request, s.servesTLS()) {
		c.JSON(http.StatusForbidden, gin.H{"error": "cross-origin request refused"})
		return
	}
	path := s.dashboardViewsPath()
	if path == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "saved views need a usage stats file"})
		return
	}
	ifMatch := c.GetHeader("If-Match")
	if ifMatch == "" {
		c.JSON(http.StatusPreconditionRequired, gin.H{"error": "If-Match header required"})
		return
	}
	views, errDecode := usagestats.DecodeViews(c.Request.Body)
	if errDecode != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errDecode.Error()})
		return
	}
	// A value that is not a revision matches no revision.
	ifRevision, ok := parseViewsIfMatch(ifMatch)
	if !ok {
		ifRevision = -1
	}
	stored, errSave := usagestats.SaveViews(path, ifRevision, views)
	var conflict *usagestats.ViewsConflictError
	if errors.As(errSave, &conflict) {
		c.Header("ETag", viewsETag(conflict.Current.Revision))
		c.JSON(http.StatusConflict, gin.H{
			"error":    "views changed since you loaded them",
			"revision": conflict.Current.Revision,
			"current":  conflict.Current,
		})
		return
	}
	if errSave != nil {
		log.Errorf("dashboard: save views: %v", errSave)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "saved views could not be written"})
		return
	}
	c.Header("ETag", viewsETag(stored.Revision))
	c.JSON(http.StatusOK, stored)
}
