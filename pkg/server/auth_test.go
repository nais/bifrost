package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func TestValidAPIKey(t *testing.T) {
	keys := []string{"key-one", "key-two"}
	cases := []struct {
		name     string
		provided string
		want     bool
	}{
		{"matches first", "key-one", true},
		{"matches second (rotation)", "key-two", true},
		{"no match", "nope", false},
		{"empty provided", "", false},
		{"prefix is not enough", "key-on", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validAPIKey(tc.provided, keys); got != tc.want {
				t.Fatalf("validAPIKey(%q) = %v, want %v", tc.provided, got, tc.want)
			}
		})
	}

	if validAPIKey("anything", nil) {
		t.Fatal("no configured keys must never validate")
	}
}

func TestExtractAPIKey(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"bearer", map[string]string{"Authorization": "Bearer secret"}, "secret"},
		{"bearer case-insensitive", map[string]string{"Authorization": "bearer secret"}, "secret"},
		{"bare authorization", map[string]string{"Authorization": "secret"}, "secret"},
		{"x-api-key", map[string]string{"X-API-Key": "secret"}, "secret"},
		{"authorization wins over x-api-key", map[string]string{"Authorization": "Bearer a", "X-API-Key": "b"}, "a"},
		{"none", map[string]string{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/unleash", nil)
			for k, v := range tc.headers {
				c.Request.Header.Set(k, v)
			}
			if got := extractAPIKey(c); got != tc.want {
				t.Fatalf("extractAPIKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAPIKeyAuthMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := logrus.New()
	logger.SetOutput(nopWriter{})

	newRouter := func(keys []string, enforced bool) *gin.Engine {
		r := gin.New()
		r.Use(apiKeyAuthMiddleware(keys, enforced, logger, []string{"/healthz"}))
		r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "OK") })
		r.GET("/v1/unleash", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
		return r
	}

	cases := []struct {
		name     string
		keys     []string
		enforced bool
		path     string
		header   string
		wantCode int
	}{
		{"skip path always allowed", []string{"k"}, true, "/healthz", "", http.StatusOK},
		{"enforced + valid key", []string{"k"}, true, "/v1/unleash", "Bearer k", http.StatusOK},
		{"enforced + missing key", []string{"k"}, true, "/v1/unleash", "", http.StatusUnauthorized},
		{"enforced + wrong key", []string{"k"}, true, "/v1/unleash", "Bearer x", http.StatusUnauthorized},
		{"not enforced + missing key allowed", []string{"k"}, false, "/v1/unleash", "", http.StatusOK},
		{"not enforced + valid key allowed", []string{"k"}, false, "/v1/unleash", "Bearer k", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			newRouter(tc.keys, tc.enforced).ServeHTTP(w, req)
			if w.Code != tc.wantCode {
				t.Fatalf("%s: got %d, want %d", tc.name, w.Code, tc.wantCode)
			}
		})
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
