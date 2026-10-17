package cmd

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/server"
)

// On a signal the server states are saved and the HTTP server stops, so that
// Agent exits instead of being killed.
func TestShutdownOnSignalSavesStatesAndStopsServer(t *testing.T) {
	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	require.NoError(t, err)
	cfg.AuthenticationToken = "test-token"
	cfg.System.RootDirectory = t.TempDir()
	config.Set(cfg)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{ReadHeaderTimeout: time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()

	signals := make(chan os.Signal, 1)
	go shutdownOnSignal(signals, server.NewEmptyManager(nil), srv)
	signals <- syscall.SIGTERM

	select {
	case err := <-served:
		require.ErrorIs(t, err, http.ErrServerClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("expected the HTTP server to stop")
	}
	_, err = os.Stat(config.Get().System.GetStatesPath())
	require.NoError(t, err, "expected the server states to be saved")
}

func TestShutdownOnSignalWaitsForActiveResponse(t *testing.T) {
	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	require.NoError(t, err)
	cfg.AuthenticationToken = "test-token"
	cfg.System.RootDirectory = t.TempDir()
	config.Set(cfg)
	started := make(chan struct{})
	finish := make(chan struct{})
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-finish
		_, _ = w.Write([]byte("complete"))
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer srv.Close()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()
	response := make(chan string, 1)
	go func() {
		res, err := http.Get("http://" + listener.Addr().String())
		if err != nil {
			response <- err.Error()
			return
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		response <- string(b)
	}()
	<-started
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() { defer close(done); shutdownOnSignal(signals, server.NewEmptyManager(nil), srv) }()
	signals <- syscall.SIGTERM
	require.ErrorIs(t, <-served, http.ErrServerClosed)
	select {
	case <-done:
		close(finish)
		t.Fatal("shutdown finished before the active response")
	default:
	}
	close(finish)
	require.Equal(t, "complete", <-response)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish after the response")
	}
}
