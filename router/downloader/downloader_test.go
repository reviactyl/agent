package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/remote"
	"github.com/reviactyl/agent/server"
	"github.com/reviactyl/agent/server/filesystem"
)

func TestExecuteStreamingResponse(t *testing.T) {
	cfg := &config.Configuration{AuthenticationToken: "test", System: config.SystemConfiguration{Data: t.TempDir(), DiskCheckInterval: 60}}
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	config.Set(cfg)
	s, err := server.NewEmptyManager(nil).InitServer("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", remote.ServerConfigurationResponse{Settings: []byte(`{"uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}`)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CleanupForDestroy)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="stream.txt"`)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "streamed contents")
	}))
	defer origin.Close()
	previousClient := client
	client = origin.Client()
	t.Cleanup(func() { client = previousClient })
	u, _ := url.Parse(origin.URL + "/download")
	for _, quota := range []int64{0, 8} {
		t.Run(fmt.Sprintf("quota=%d", quota), func(t *testing.T) {
			s.Filesystem().SetDiskLimit(quota)
			dl := New(s, DownloadRequest{URL: u, Directory: "/nested", UseHeader: true})
			err := dl.Execute()
			if quota > 0 {
				data, readErr := os.ReadFile(filepath.Join(s.Filesystem().Path(), "nested", "stream.txt"))
				if readErr != nil || string(data) != "streamed contents" {
					t.Fatalf("failed download changed original: %q, %v", data, readErr)
				}
				if !filesystem.IsErrorCode(err, filesystem.ErrCodeDiskSpace) {
					t.Fatalf("expected quota error, got %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(s.Filesystem().Path(), "nested", "stream.txt"))
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != "streamed contents" {
					t.Fatalf("unexpected contents: %q", data)
				}
				if dl.Progress() != 1 {
					t.Fatalf("completion progress = %v", dl.Progress())
				}
			}
			if quota > 0 && dl.Progress() != 0 {
				t.Fatalf("failed download progress = %v", dl.Progress())
			}
			if usage := s.Filesystem().CachedUsage(); usage != int64(len("streamed contents")) {
				t.Fatalf("usage = %d", usage)
			}
			if ByID(dl.Identifier) != nil {
				t.Fatal("download remains tracked")
			}
		})
	}
}

func TestCounterUnknownLength(t *testing.T) {
	for _, length := range []int64{-1, 0, 10} {
		dl := &Download{}
		_, _ = dl.counter(length).Write([]byte("12345"))
		want := 0.0
		if length > 0 {
			want = 0.5
		}
		if dl.Progress() != want {
			t.Fatalf("length %d: progress = %v, want %v", length, dl.Progress(), want)
		}
		if _, err := dl.MarshalJSON(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetainedFailuresReleaseSlotsAndExpire(t *testing.T) {
	s := newPullTestServer(t)
	t.Cleanup(func() {
		for _, dl := range ByServer(s.ID()) {
			dl.Cancel()
		}
	})
	d := instance
	var first, last *Download
	for i := 0; i <= maxRetainedFailures; i++ {
		dl := New(s, DownloadRequest{RetainFailure: true})
		require.NotNil(t, dl)
		if i == 0 {
			first = dl
		}
		d.complete(dl, &responseStatusError{code: http.StatusNotFound})
		last = dl
	}
	require.Nil(t, ByID(first.Identifier), "oldest failure must be evicted")
	require.Len(t, ByServer(s.ID()), maxRetainedFailures)
	for range 3 {
		require.NotNil(t, New(s, DownloadRequest{}), "failure history must not consume slots")
	}
	require.Nil(t, New(s, DownloadRequest{}), "active admission limit must remain three")
	d.mu.Lock()
	d.failures[last.Identifier].timer.Reset(time.Millisecond)
	d.mu.Unlock()
	require.Eventually(t, func() bool { return ByID(last.Identifier) == nil }, time.Second, time.Millisecond)
	// Explicit dismissal removes retained history and its timer too.
	for _, dl := range ByServer(s.ID()) {
		dl.Cancel()
	}
	require.Empty(t, ByServer(s.ID()))
	d.mu.RLock()
	require.Empty(t, d.serverCache[s.ID()])
	d.mu.RUnlock()
}

func TestCancelDoesNotRestoreFailureHistory(t *testing.T) {
	s := newPullTestServer(t)
	for range 50 {
		dl := New(s, DownloadRequest{RetainFailure: true})
		require.NotNil(t, dl)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); dl.Cancel() }()
		go func() {
			defer wg.Done()
			instance.complete(dl, io.ErrUnexpectedEOF)
			_, _ = json.Marshal(dl)
		}()
		wg.Wait()
		require.Nil(t, ByID(dl.Identifier))
	}
}

func TestFailureMessagesDoNotExposeURLCredentials(t *testing.T) {
	err := fmt.Errorf("%w: %w", ErrDownloadFailed, &url.Error{Op: "Get", URL: "http://user:secret@example.com/?token=private", Err: io.EOF})
	message := failureMessage(err)
	require.Contains(t, message, "reachable")
	require.NotContains(t, message, "secret")
	require.NotContains(t, message, "private")
}

func TestCancelRunningBackgroundDownload(t *testing.T) {
	s := newPullTestServer(t)
	started := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(origin.Close)
	u, err := url.Parse(origin.URL)
	require.NoError(t, err)
	dl := New(s, DownloadRequest{URL: u, RetainFailure: true})
	t.Cleanup(dl.Cancel)
	result := make(chan error, 1)
	go func() { result <- dl.Execute() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}
	dl.Cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled download did not finish")
	}
	require.Nil(t, ByID(dl.Identifier), "cancellation must not produce failure history")
}

func TestFailureMessageForDirectoryWriteRejection(t *testing.T) {
	err := fmt.Errorf("write: %w", &os.LinkError{Op: "rename", Old: "/private/staging", New: "/private/destination", Err: syscall.EISDIR})
	require.Equal(t, "The download destination is an existing directory. Choose a different filename.", failureMessage(err))
}
