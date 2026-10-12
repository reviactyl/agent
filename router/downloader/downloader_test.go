package downloader

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

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
	s, err := server.NewEmptyManager(nil).InitServer(remote.ServerConfigurationResponse{Settings: []byte(`{"uuid":"stream-test"}`)})
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
			if s.Filesystem().CachedUsage() > quota && quota > 0 {
				t.Fatal("disk quota exceeded")
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
