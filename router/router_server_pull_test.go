package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/remote"
	"github.com/reviactyl/agent/router/downloader"
	"github.com/reviactyl/agent/router/middleware"
	"github.com/reviactyl/agent/server"
)

func TestPullRemoteFileConcurrentAdmission(t *testing.T) {
	previous := config.Get()
	t.Cleanup(func() { config.Set(previous) })
	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	require.NoError(t, err)
	cfg.AuthenticationToken = "test"
	cfg.System.Data = t.TempDir()
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	cfg.Api.RemoteDownloadAllowlist = []string{"127.0.0.1"}
	config.Set(cfg)
	const id = "12712712-7127-4127-8127-127127127127"
	s, err := server.NewEmptyManager(nil).InitServer(id, remote.ServerConfigurationResponse{Settings: []byte(`{"uuid":"` + id + `"}`)})
	require.NoError(t, err)
	t.Cleanup(s.CleanupForDestroy)

	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			_, _ = w.Write([]byte("downloaded"))
		case <-r.Context().Done():
		}
	}))
	defer origin.Close()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	gin.SetMode(gin.TestMode)
	pull := func(name string, foreground bool) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"root":"/","url":%q,"file_name":%q,"foreground":%t}`, origin.URL+"/file", name, foreground)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/pull", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("server", s)
		postServerPullRemoteFile(c)
		middleware.CaptureErrors()(c)
		return w
	}
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 4)
	for i := range 4 {
		go func() {
			<-start
			results <- pull(fmt.Sprintf("file-%d", i), false)
		}()
	}
	close(start)
	admitted, rejected := 0, 0
	for range 4 {
		w := <-results
		switch w.Code {
		case http.StatusAccepted:
			admitted++
			var response struct{ Identifier string }
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			require.NotNil(t, downloader.ByID(response.Identifier))
		case http.StatusBadRequest:
			rejected++
			require.Contains(t, w.Body.String(), "limit of 3")
		default:
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
	}
	require.Equal(t, 3, admitted)
	require.Equal(t, 1, rejected)
	require.Len(t, downloader.ByServer(s.ID()), 3)
	unblock()
	require.Eventually(t, func() bool { return len(downloader.ByServer(s.ID())) == 0 }, time.Second, time.Millisecond)
	w := pull("completed", true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Eventually(t, func() bool { return len(downloader.ByServer(s.ID())) == 0 }, time.Second, time.Millisecond)
	contents, err := os.ReadFile(filepath.Join(s.Filesystem().Path(), "completed"))
	require.NoError(t, err)
	require.Equal(t, "downloaded", string(contents))
}

func TestBackgroundPullFailureRemainsObservable(t *testing.T) {
	previous := config.Get()
	t.Cleanup(func() { config.Set(previous) })
	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	require.NoError(t, err)
	cfg.AuthenticationToken = "test"
	cfg.System.Data = t.TempDir()
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	cfg.Api.RemoteDownloadAllowlist = []string{"127.0.0.1"}
	config.Set(cfg)
	const id = "12812812-8128-4128-8128-128128128128"
	s, err := server.NewEmptyManager(nil).InitServer(id, remote.ServerConfigurationResponse{Settings: []byte(`{"uuid":"` + id + `"}`)})
	require.NoError(t, err)
	t.Cleanup(s.CleanupForDestroy)
	t.Cleanup(func() {
		for _, dl := range downloader.ByServer(s.ID()) {
			dl.Cancel()
		}
	})
	require.NoError(t, os.MkdirAll(filepath.Join(s.Filesystem().Path(), "directory"), 0o755))
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path == "/short" {
			w.Header().Set("Content-Length", "100")
		}
		_, _ = w.Write([]byte("downloaded"))
	}))
	defer origin.Close()
	gin.SetMode(gin.TestMode)
	pull := func(path, name string, foreground bool) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/pull", strings.NewReader(fmt.Sprintf(`{"root":"/","url":%q,"file_name":%q,"foreground":%t}`, origin.URL+path, name, foreground)))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("server", s)
		postServerPullRemoteFile(c)
		middleware.CaptureErrors()(c)
		return w
	}
	for _, tt := range []struct{ path, name, message string }{
		{"/missing", "missing", "404"},
		{"/ok", "directory", "directory"},
		{"/short", "short", "incomplete"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := pull(tt.path, tt.name, false)
			require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
			var accepted struct{ Identifier string }
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &accepted))
			require.Eventually(t, func() bool {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Set("server", s)
				getServerPullingFiles(c)
				var response struct {
					Downloads []struct {
						Identifier, Status, Error string
						Progress                  float64
					}
				}
				if json.Unmarshal(w.Body.Bytes(), &response) != nil {
					return false
				}
				for _, dl := range response.Downloads {
					if dl.Identifier == accepted.Identifier && dl.Status == "failed" {
						return strings.Contains(dl.Error, tt.message)
					}
				}
				return false
			}, 2*time.Second, time.Millisecond, "failed background download must remain observable with useful error")
			require.NotNil(t, downloader.ByID(accepted.Identifier))
		})
	}
	// Retained failures must not consume any of the three admission slots.
	w := pull("/ok", "success", false)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	var accepted struct{ Identifier string }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &accepted))
	require.Eventually(t, func() bool { return downloader.ByID(accepted.Identifier) == nil }, time.Second, time.Millisecond)
	contents, err := os.ReadFile(filepath.Join(s.Filesystem().Path(), "success"))
	require.NoError(t, err)
	require.Equal(t, "downloaded", string(contents))
	require.Len(t, downloader.ByServer(s.ID()), 3)
	w = pull("/missing", "foreground", true)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Len(t, downloader.ByServer(s.ID()), 3, "foreground errors must not enter background history")
	other, err := server.NewEmptyManager(nil).InitServer("12812812-8128-4128-8128-128128128129", remote.ServerConfigurationResponse{Settings: []byte(`{"uuid":"12812812-8128-4128-8128-128128128129"}`)})
	require.NoError(t, err)
	t.Cleanup(other.CleanupForDestroy)
	require.Empty(t, downloader.ByServer(other.ID()), "failure history must be scoped to its server")
	for _, dl := range downloader.ByServer(s.ID()) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("server", other)
		c.Params = gin.Params{{Key: "download", Value: dl.Identifier}}
		deleteServerPullRemoteFile(c)
		require.NotNil(t, downloader.ByID(dl.Identifier), "another server must not dismiss this failure")
		c.Set("server", s)
		deleteServerPullRemoteFile(c)
		require.Nil(t, downloader.ByID(dl.Identifier), "owner must be able to dismiss this failure")
	}
}
