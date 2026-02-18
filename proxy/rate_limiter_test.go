package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestRateLimiter_DisabledByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("LLAMA_SWAP_RATE_LIMIT_RPM", "")

	rl := newProxyRateLimiter(NewLogMonitorWriter(io.Discard))
	r := gin.New()
	r.Use(rl.middleware())
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	}
}

func TestRateLimiter_RejectsBurstOverflow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("LLAMA_SWAP_RATE_LIMIT_RPM", "60")
	t.Setenv("LLAMA_SWAP_RATE_LIMIT_BURST", "1")
	t.Setenv("LLAMA_SWAP_RATE_LIMIT_TTL_SECONDS", "600")

	rl := newProxyRateLimiter(NewLogMonitorWriter(io.Discard))
	r := gin.New()
	r.Use(rl.middleware())
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	firstReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	firstRes := httptest.NewRecorder()
	r.ServeHTTP(firstRes, firstReq)
	assert.Equal(t, http.StatusOK, firstRes.Code)

	secondReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	secondRes := httptest.NewRecorder()
	r.ServeHTTP(secondRes, secondReq)
	assert.Equal(t, http.StatusTooManyRequests, secondRes.Code)
}

func TestRateLimiter_DoesNotAffectAdminPOST(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("LLAMA_SWAP_RATE_LIMIT_RPM", "60")
	t.Setenv("LLAMA_SWAP_RATE_LIMIT_BURST", "1")

	rl := newProxyRateLimiter(NewLogMonitorWriter(io.Discard))
	r := gin.New()
	r.Use(rl.middleware())
	r.POST("/api/recipes/models", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/recipes/models", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	}
}
