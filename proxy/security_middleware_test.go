package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestSecurityHeadersMiddleware_DefaultEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("LLAMA_SWAP_SECURITY_HEADERS", "")

	pm := &ProxyManager{}
	r := gin.New()
	r.Use(pm.securityHeadersMiddleware())
	r.GET("/health", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"))
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "no-referrer", w.Header().Get("Referrer-Policy"))
}

func TestSecurityHeadersMiddleware_HSTSWhenHttpsForwarded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("LLAMA_SWAP_SECURITY_HEADERS", "1")

	pm := &ProxyManager{}
	r := gin.New()
	r.Use(pm.securityHeadersMiddleware())
	r.GET("/health", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "max-age=31536000; includeSubDomains", w.Header().Get("Strict-Transport-Security"))
}
