package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/reviactyl/agent/remote"
	"github.com/reviactyl/agent/server"
)

func TestDeleteFilesCanceledRequestPreservesFiles(t *testing.T) {
	u := newUploadTest(t, 1, 0)
	s, err := server.NewEmptyManager(nil).InitServer(uploadTestServerUuid, remote.ServerConfigurationResponse{
		Settings: []byte(fmt.Sprintf(`{"uuid":%q}`, uploadTestServerUuid)), ProcessConfiguration: &remote.ProcessConfiguration{},
	})
	require.NoError(t, err)
	t.Cleanup(s.CtxCancel)
	require.NoError(t, os.WriteFile(filepath.Join(u.root, "keep.txt"), []byte("keep"), 0o644))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("server", s)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"root":"/","files":["keep.txt"]}`)).WithContext(ctx)
	postServerDeleteFiles(c)
	require.NotEmpty(t, c.Errors)
	require.ErrorIs(t, c.Errors.Last().Err, context.Canceled)
	content, err := os.ReadFile(filepath.Join(u.root, "keep.txt"))
	require.NoError(t, err)
	require.Equal(t, "keep", string(content))
}

func TestDeleteFilesStopsLaterEntries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	files := make([]string, 20)
	for i := range files {
		files[i] = fmt.Sprintf("file-%02d", i)
		require.NoError(t, os.WriteFile(filepath.Join(root, files[i]), []byte("fixture"), 0o600))
	}
	started := make(chan string, len(files))
	release := make(chan struct{})
	var calls atomic.Int32
	result := make(chan error, 1)
	go func() {
		result <- deleteFiles(ctx, root, files, func(p string) error {
			calls.Add(1)
			started <- p
			<-release
			return os.Remove(p)
		})
	}()
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	defer finish()
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("deletion workers did not start")
		}
	}
	cancel()
	finish()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("deletion group did not finish")
	}
	require.EqualValues(t, 4, calls.Load())
	for _, file := range files[:4] {
		require.NoFileExists(t, filepath.Join(root, file))
	}
	for _, file := range files[4:] {
		require.FileExists(t, filepath.Join(root, file))
	}
}

func TestDeleteFilesPropagatesFailure(t *testing.T) {
	failure := errors.New("delete failed")
	paths := make(chan string, 1)
	require.ErrorIs(t, deleteFiles(context.Background(), "/root", []string{"file"}, func(p string) error {
		paths <- p
		return failure
	}), failure)
	require.Equal(t, "/root/file", <-paths)
}

func TestDeleteFilesSuccessAndMissingPaths(t *testing.T) {
	u := newUploadTest(t, 1, 0)
	require.NoError(t, os.MkdirAll(filepath.Join(u.root, "nested", "directory"), 0o755))
	for _, file := range []string{"nested/file", "nested/directory/child", "keep"} {
		require.NoError(t, os.WriteFile(filepath.Join(u.root, file), []byte("fixture"), 0o600))
	}
	req, err := http.NewRequest(http.MethodPost, u.url+"/api/servers/"+uploadTestServerUuid+"/files/delete", strings.NewReader(`{"root":"/nested","files":["file","directory","missing"]}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	require.NoFileExists(t, filepath.Join(u.root, "nested/file"))
	require.NoDirExists(t, filepath.Join(u.root, "nested/directory"))
	require.FileExists(t, filepath.Join(u.root, "keep"))
}
