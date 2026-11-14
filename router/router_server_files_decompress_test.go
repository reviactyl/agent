package router

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/remote"
	"github.com/reviactyl/agent/server"
	"github.com/reviactyl/agent/server/filesystem"
)

func TestDecompressRequestCancellation(t *testing.T) {
	for _, diskLimit := range []int64{0, 1 << 20} {
		name := "extraction"
		if diskLimit > 0 {
			name = "space check"
		}
		t.Run(name, func(t *testing.T) {
			c, s := newDecompressContext(t)
			s.Filesystem().SetDiskLimit(diskLimit)
			ctx, cancel := context.WithCancel(c.Request.Context())
			cancel()
			c.Request = c.Request.WithContext(ctx)

			postServerDecompressFiles(c)

			require.NotEmpty(t, c.Errors)
			require.ErrorIs(t, c.Errors.Last().Err, context.Canceled)
			_, err := os.Stat(filepath.Join(s.Filesystem().Path(), "extracted.txt"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestDecompressRequestPreservesSuccessAndSpaceCheck(t *testing.T) {
	for _, tc := range []struct {
		name      string
		limit     int64
		wantError bool
	}{
		{name: "success", limit: 1 << 20},
		{name: "insufficient space", limit: 1, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, s := newDecompressContext(t)
			s.Filesystem().SetDiskLimit(tc.limit)

			postServerDecompressFiles(c)

			p := filepath.Join(s.Filesystem().Path(), "extracted.txt")
			if tc.wantError {
				require.NotEmpty(t, c.Errors)
				require.True(t, filesystem.IsErrorCode(c.Errors.Last().Err, filesystem.ErrCodeDiskSpace))
				_, err := os.Stat(p)
				require.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.Empty(t, c.Errors)
			require.Equal(t, http.StatusNoContent, c.Writer.Status())
			content, err := os.ReadFile(p)
			require.NoError(t, err)
			require.Equal(t, "hello", string(content))
		})
	}
}

func newDecompressContext(t *testing.T) (*gin.Context, *server.Server) {
	t.Helper()
	previous := config.Get()
	t.Cleanup(func() { config.Set(previous) })
	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	require.NoError(t, err)
	cfg.AuthenticationToken = "test-token"
	cfg.System.Data = t.TempDir()
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	config.Set(cfg)
	const uuid = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	settings, err := json.Marshal(map[string]any{"uuid": uuid})
	require.NoError(t, err)
	s, err := server.NewEmptyManager(nil).InitServer(uuid, remote.ServerConfigurationResponse{
		Settings: settings, ProcessConfiguration: &remote.ProcessConfiguration{},
	})
	require.NoError(t, err)
	t.Cleanup(s.CtxCancel)
	t.Cleanup(func() { _ = s.Filesystem().UnixFS().Close() })
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "extracted.txt", Mode: 0o644, Size: 5}))
	_, err = tw.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, os.WriteFile(filepath.Join(s.Filesystem().Path(), "fixture.tar"), archive.Bytes(), 0o600))
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/files/decompress", strings.NewReader(`{"root":"/","file":"fixture.tar"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("server", s)
	c.Set("logger", log.WithField("test", t.Name()))
	return c, s
}
