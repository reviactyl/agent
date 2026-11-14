package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/remote"
	"github.com/reviactyl/agent/server"
)

func TestRemoteDownloadRuntimeConfiguration(t *testing.T) {
	t.Setenv("AGENT_TOKEN_ID", "")
	t.Setenv("AGENT_TOKEN", "")
	previous := config.Get()
	t.Cleanup(func() { config.Set(previous) })

	for _, initiallyDisabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("initially_disabled_%t", initiallyDisabled), func(t *testing.T) {
			cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
			require.NoError(t, err)
			cfg.AuthenticationTokenId = "test-id"
			cfg.AuthenticationToken = "test-token"
			cfg.System.Data = t.TempDir()
			cfg.Api.DisableRemoteDownload = initiallyDisabled
			require.NoError(t, cfg.ResolveToken(false))
			config.Set(cfg)
			manager := server.NewEmptyManager(backupTestRemoteClient{})
			const id = "14114114-1141-4141-8141-141141141141"
			s, err := manager.InitServer(id, remote.ServerConfigurationResponse{Settings: []byte(`{"uuid":"` + id + `"}`)})
			require.NoError(t, err)
			t.Cleanup(s.CleanupForDestroy)
			manager.Add(s)
			engine := Configure(manager, manager.Client())
			request := func(method, path, token, body string) *httptest.ResponseRecorder {
				t.Helper()
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				rec := httptest.NewRecorder()
				engine.ServeHTTP(rec, req)
				return rec
			}
			for _, disabled := range []bool{initiallyDisabled, !initiallyDisabled, initiallyDisabled} {
				config.Update(func(c *config.Configuration) { c.Api.DisableRemoteDownload = disabled })
				// Panel updates replace the configuration but retain this operator-owned flag.
				update := request(http.MethodPost, "/api/update", "test-token", `{}`)
				require.Equal(t, http.StatusOK, update.Code, update.Body.String())
				require.JSONEq(t, `{"applied":true}`, update.Body.String())
				require.Equal(t, disabled, config.Get().Api.DisableRemoteDownload)

				for _, route := range []struct {
					method string
					suffix string
					status int
				}{
					{http.MethodGet, "", http.StatusOK},
					{http.MethodPost, "", http.StatusBadRequest},
					{http.MethodDelete, "/14114114-1141-4141-8141-141141141141", http.StatusNoContent},
				} {
					path := "/api/servers/" + id + "/files/pull" + route.suffix
					require.Equal(t, http.StatusUnauthorized, request(route.method, path, "", `{}`).Code)
					require.Equal(t, http.StatusForbidden, request(route.method, path, "wrong-token", `{}`).Code)
					rec := request(route.method, path, "test-token", `{}`)
					if disabled {
						require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
						require.JSONEq(t, `{"error":"This functionality is not currently enabled on this instance."}`, rec.Body.String())
					} else {
						require.Equal(t, route.status, rec.Code, rec.Body.String())
						require.NotContains(t, rec.Body.String(), "This functionality is not currently enabled")
					}
				}
			}
			var updates sync.WaitGroup
			updates.Go(func() {
				for i := range 1000 {
					config.Update(func(c *config.Configuration) { c.Api.DisableRemoteDownload = i%2 == 0 })
				}
			})
			defer updates.Wait()
			for range 100 {
				rec := request(http.MethodGet, "/api/servers/"+id+"/files/pull", "test-token", "")
				require.Contains(t, []int{http.StatusOK, http.StatusBadRequest}, rec.Code)
			}
		})
	}
}
