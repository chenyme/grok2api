package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

const UpstreamRequestDispositionHeader = "X-Upstream-Request-Disposition"

// AbortImagePreSubmitRefusal marks only exact image submission endpoints when
// server-owned control flow proves that the generation or edit payload was not
// submitted upstream. Incoming request or upstream response headers are never
// accepted as that proof.
func AbortImagePreSubmitRefusal(c *gin.Context) bool {
	if c.Request.Method != http.MethodPost {
		return false
	}
	switch c.Request.URL.Path {
	case "/v1/images/generations", "/v1/images/edits":
		c.Header(UpstreamRequestDispositionHeader, "not-submitted")
		writeOpenAIError(
			c,
			http.StatusServiceUnavailable,
			"upstream_not_submitted",
			"Image request was refused before provider submission.",
		)
		return true
	default:
		return false
	}
}
