package proxy

import (
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

func securityHeadersEnabledFromEnv() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LLAMA_SWAP_SECURITY_HEADERS")))
	if v == "" {
		return true
	}
	return v != "0" && v != "false" && v != "no"
}

func (pm *ProxyManager) securityHeadersMiddleware() gin.HandlerFunc {
	enabled := securityHeadersEnabledFromEnv()
	return func(c *gin.Context) {
		if enabled {
			c.Header("X-Frame-Options", "DENY")
			c.Header("X-Content-Type-Options", "nosniff")
			c.Header("Referrer-Policy", "no-referrer")
			c.Header("Permissions-Policy", "geolocation=(), microphone=(), camera=()")

			proto := strings.ToLower(strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")))
			if c.Request.TLS != nil || proto == "https" {
				c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
		}
		c.Next()
	}
}
