package proxy

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

const (
	defaultRateLimitBurst      = 5
	defaultRateLimitTTLSeconds = 600
	minRateLimitTTLSeconds     = 30
	rateLimitExceededMessage   = "rate limit exceeded"
)

type clientRateState struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type proxyRateLimiter struct {
	enabled bool
	limit   rate.Limit
	burst   int
	ttl     time.Duration

	mu      sync.Mutex
	clients map[string]*clientRateState

	logger *LogMonitor
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

func newProxyRateLimiter(logger *LogMonitor) *proxyRateLimiter {
	rpm := envInt("LLAMA_SWAP_RATE_LIMIT_RPM", 0)
	if rpm <= 0 {
		return &proxyRateLimiter{enabled: false, logger: logger}
	}

	burst := envInt("LLAMA_SWAP_RATE_LIMIT_BURST", defaultRateLimitBurst)
	if burst < 1 {
		burst = 1
	}

	ttlSeconds := envInt("LLAMA_SWAP_RATE_LIMIT_TTL_SECONDS", defaultRateLimitTTLSeconds)
	if ttlSeconds < minRateLimitTTLSeconds {
		ttlSeconds = minRateLimitTTLSeconds
	}

	rl := &proxyRateLimiter{
		enabled: true,
		limit:   rate.Limit(float64(rpm) / 60.0),
		burst:   burst,
		ttl:     time.Duration(ttlSeconds) * time.Second,
		clients: make(map[string]*clientRateState),
		logger:  logger,
	}

	if rl.logger != nil {
		rl.logger.Infof("Rate limiting enabled (rpm=%d burst=%d ttl=%ds)", rpm, burst, ttlSeconds)
	}

	return rl
}

func shouldRateLimitRequest(c *gin.Context) bool {
	if c.Request.Method == http.MethodOptions {
		return false
	}
	if c.Request.Method != http.MethodPost {
		return false
	}

	path := c.Request.URL.Path
	switch path {
	case "/v1/chat/completions",
		"/v1/responses",
		"/v1/completions",
		"/v1/messages",
		"/v1/messages/count_tokens",
		"/v1/embeddings",
		"/reranking",
		"/rerank",
		"/v1/rerank",
		"/v1/reranking",
		"/infill",
		"/completion",
		"/v1/audio/speech",
		"/v1/audio/voices",
		"/v1/audio/transcriptions",
		"/v1/images/generations",
		"/v1/images/edits":
		return true
	}

	return false
}

func (rl *proxyRateLimiter) allow(clientKey string) bool {
	now := time.Now()
	cutoff := now.Add(-rl.ttl)

	rl.mu.Lock()
	defer rl.mu.Unlock()

	for key, state := range rl.clients {
		if state.lastSeen.Before(cutoff) {
			delete(rl.clients, key)
		}
	}

	state, ok := rl.clients[clientKey]
	if !ok {
		state = &clientRateState{limiter: rate.NewLimiter(rl.limit, rl.burst)}
		rl.clients[clientKey] = state
	}
	state.lastSeen = now

	return state.limiter.Allow()
}

func (rl *proxyRateLimiter) middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if rl == nil || !rl.enabled || !shouldRateLimitRequest(c) {
			c.Next()
			return
		}

		clientIP := strings.TrimSpace(c.ClientIP())
		if clientIP == "" {
			clientIP = "unknown"
		}

		if !rl.allow(clientIP) {
			c.Header("Retry-After", "1")
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": rateLimitExceededMessage,
			})
			return
		}

		c.Next()
	}
}
