package middleware

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/reviactyl/agent/config"
)

func newCORSTestEngine(t *testing.T, configure func(cfg *config.Configuration)) *gin.Engine {
	t.Helper()
	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	require.NoError(t, err)
	cfg.AuthenticationToken = "test-token"
	cfg.PanelLocation = "http://panel.test"
	configure(cfg)
	config.Set(cfg)

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(SetAccessControlHeaders())
	engine.GET("/resource", func(c *gin.Context) { c.Status(http.StatusOK) })
	return engine
}

// The private network grant is sent under the response header name browsers
// read.
func TestPrivateNetworkPreflightGrantUsesResponseHeader(t *testing.T) {
	engine := newCORSTestEngine(t, func(cfg *config.Configuration) {
		cfg.AllowCORSPrivateNetwork = true
	})

	req := httptest.NewRequest(http.MethodOptions, "/resource", nil)
	req.Header.Set("Origin", "http://panel.test")
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	require.Equal(t, "true", rec.Header().Get("Access-Control-Allow-Private-Network"))
}

func TestCORSUsesCurrentConfiguration(t *testing.T) {
	engine := newCORSTestEngine(t, func(cfg *config.Configuration) {
		cfg.AllowedOrigins = []string{"https://old.test"}
		cfg.AllowCORSPrivateNetwork = true
	})
	config.Update(func(cfg *config.Configuration) {
		cfg.PanelLocation = "https://new-panel.test"
		cfg.AllowedOrigins = []string{"https://new.test"}
		cfg.AllowCORSPrivateNetwork = false
	})
	for _, origin := range []string{"https://new-panel.test", "https://new.test", "https://old.test", "http://panel.test"} {
		req := httptest.NewRequest(http.MethodOptions, "/resource", nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		want := "https://new-panel.test"
		if origin == "https://new.test" {
			want = origin
		}
		require.Equal(t, http.StatusNoContent, rec.Code)
		require.Equal(t, want, rec.Header().Get("Access-Control-Allow-Origin"))
		require.Empty(t, rec.Header().Get("Access-Control-Allow-Private-Network"))
	}
}
