package server

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// extractAPIKey returns the pre-shared key presented on the request, accepting
// either "Authorization: Bearer <key>" (or a bare "Authorization: <key>") or an
// "X-API-Key: <key>" header.
func extractAPIKey(c *gin.Context) string {
	if h := c.GetHeader("Authorization"); h != "" {
		if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return strings.TrimSpace(h)
	}
	return strings.TrimSpace(c.GetHeader("X-API-Key"))
}

// validAPIKey reports whether provided matches any configured key, using a
// constant-time comparison against every key (no early return) so neither the
// match result nor which key matched is leaked by timing.
func validAPIKey(provided string, keys []string) bool {
	providedBytes := []byte(provided)
	var match int
	for _, k := range keys {
		match |= subtle.ConstantTimeCompare(providedBytes, []byte(k))
	}
	return match == 1
}

// apiKeyAuthMiddleware authenticates requests with a pre-shared key.
//
// Requests to skipPaths (health, openapi) are always allowed. When no valid key
// is presented: if enforced, the request is rejected with 401; otherwise
// ("accept-then-enforce") it is allowed but logged so unauthenticated callers
// are visible before enforcement is switched on.
func apiKeyAuthMiddleware(keys []string, enforced bool, logger *logrus.Logger, skipPaths []string) gin.HandlerFunc {
	skip := make(map[string]struct{}, len(skipPaths))
	for _, p := range skipPaths {
		skip[p] = struct{}{}
	}

	return func(c *gin.Context) {
		if _, ok := skip[c.Request.URL.Path]; ok {
			c.Next()
			return
		}

		if validAPIKey(extractAPIKey(c), keys) {
			apiAuthRequestsTotal.WithLabelValues(authOutcomeAuthenticated).Inc()
			c.Next()
			return
		}

		if enforced {
			apiAuthRequestsTotal.WithLabelValues(authOutcomeRejected).Inc()
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":       "unauthorized",
				"message":     "missing or invalid API key",
				"status_code": http.StatusUnauthorized,
			})
			return
		}

		// Dark-launch / accept mode: allow but record so the rollout is
		// measurable. When bifrost_api_auth_requests_total{outcome="unauthenticated_allowed"}
		// reaches zero, it is safe to flip BIFROST_AUTH_ENFORCED to true.
		apiAuthRequestsTotal.WithLabelValues(authOutcomeUnauthenticated).Inc()
		logger.WithFields(logrus.Fields{
			"event":     "unauthenticated_request",
			"path":      c.Request.URL.Path,
			"method":    c.Request.Method,
			"client_ip": c.ClientIP(),
		}).Warn("Request without a valid API key allowed (authentication not enforced)")
		c.Next()
	}
}
