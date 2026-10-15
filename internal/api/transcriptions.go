package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// registerTranscriptionV8Routes adds the voice-memo transcription routes to
// the key-protected v8 management group.
func (s *Server) registerTranscriptionV8Routes(v8 *gin.RouterGroup) {
	v8.GET("/transcriptions", s.transcriber.List)
	v8.POST("/transcriptions", s.transcriber.Upload)
	v8.GET("/transcriptions/:id", s.transcriber.Get)
	v8.GET("/transcriptions/:id/:format", s.transcriber.Download)
	v8.DELETE("/transcriptions/:id", s.transcriber.Delete)
}

// registerDashboardTranscriptionRoutes serves the same routes to the
// dashboard's Transcripts tab without a key, under the read-only dashboard's
// access rule.
func (s *Server) registerDashboardTranscriptionRoutes() {
	s.engine.GET("/dashboard/transcriptions", s.dashboardTranscriptions(s.transcriber.List))
	s.engine.POST("/dashboard/transcriptions", s.dashboardTranscriptions(s.transcriber.Upload))
	s.engine.GET("/dashboard/transcriptions/:id", s.dashboardTranscriptions(s.transcriber.Get))
	s.engine.GET("/dashboard/transcriptions/:id/:format", s.dashboardTranscriptions(s.transcriber.Download))
	s.engine.DELETE("/dashboard/transcriptions/:id", s.dashboardTranscriptions(s.transcriber.Delete))
}

// dashboardTranscriptions lets a dashboard on this host reach a transcription
// route. Uploads and deletes must also come from a page this server served,
// as saving views must.
func (s *Server) dashboardTranscriptions(h gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.dashboardViewsAllowed(c) {
			return
		}
		if c.Request.Method != http.MethodGet && !sameOrigin(c.Request, s.servesTLS()) {
			c.JSON(http.StatusForbidden, gin.H{"error": "cross-origin request refused"})
			return
		}
		h(c)
	}
}
