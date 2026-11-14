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
