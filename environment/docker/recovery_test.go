package docker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/environment"
	"github.com/reviactyl/agent/events"
	"github.com/reviactyl/agent/system"
	"github.com/stretchr/testify/require"
)

func TestRecoveryKeepsWatchingAfterDockerFailures(t *testing.T) {
	cfg, err := config.NewAtPath(t.TempDir() + "/config.yml")
	require.NoError(t, err)
	cfg.AuthenticationToken = "test-token"
	cfg.Docker.UsePerformantInspect = false
	config.Set(cfg)
	for _, inspectFailure := range []bool{false, true} {
		t.Run(fmt.Sprint("inspectFailure=", inspectFailure), func(t *testing.T) {
			var requests atomic.Int32
			var kills atomic.Int32
			docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/json"):
					n := requests.Add(1)
					if inspectFailure && n == 1 {
						http.Error(w, "unavailable", http.StatusInternalServerError)
						return
					}
					if n > 3 {
						_, _ = w.Write([]byte(`{"Id":"test","State":{"Running":false}}`))
						return
					}
					_, _ = w.Write([]byte(`{"Id":"test","State":{"Running":true}}`))
				case strings.HasSuffix(r.URL.Path, "/kill"):
					kills.Add(1)
					http.Error(w, "unavailable", http.StatusInternalServerError)
				default:
					http.Error(w, "unavailable", http.StatusInternalServerError)
				}
			}))
			defer docker.Close()
			cli, err := client.NewClientWithOpts(client.WithHost(strings.Replace(docker.URL, "http://", "tcp://", 1)), client.WithVersion("1.45"))
			require.NoError(t, err)
			defer cli.Close()
			e := &Environment{Id: "test", client: cli, meta: &Metadata{}, st: system.NewAtomicString(environment.ProcessRunningState), emitter: events.NewBus()}
			done := make(chan bool, 1)
			go func() { done <- e.reattach("test", 0) }()
			select {
			case attached := <-done:
				require.False(t, attached, "recovery ends only after observing the stopped container")
				require.Greater(t, requests.Load(), int32(3))
				if !inspectFailure {
					require.Positive(t, kills.Load())
				}
			case <-time.After(8 * time.Second):
				t.Fatal("recovery did not observe container stopping")
			}
		})
	}
}

func TestStaleRecoveryCannotKillNewAttachment(t *testing.T) {
	cfg, err := config.NewAtPath(t.TempDir() + "/config.yml")
	require.NoError(t, err)
	cfg.AuthenticationToken = "test-token"
	cfg.Docker.UsePerformantInspect = false
	cfg.Docker.CpuBurst.Enabled = false
	config.Set(cfg)
	var replacement atomic.Bool
	var replacementKills atomic.Int32
	killStarted := make(chan struct{})
	finishKill := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(finishKill) }) })
	var killOnce sync.Once
	connections := make(chan net.Conn, 1)
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			if strings.Contains(r.URL.Path, "/old/") && replacement.Load() {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			id := "old"
			if replacement.Load() {
				id = "new"
			}
			_, _ = fmt.Fprintf(w, `{"Id":%q,"State":{"Running":true}}`, id)
		case strings.HasSuffix(r.URL.Path, "/kill"):
			if strings.Contains(r.URL.Path, "/old/") || !replacement.Load() {
				killOnce.Do(func() { close(killStarted) })
				<-finishKill
				http.Error(w, "unavailable", http.StatusInternalServerError)
			} else {
				replacementKills.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}
		case strings.HasSuffix(r.URL.Path, "/attach"):
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n")
			_ = rw.Flush()
			connections <- conn
		default:
			http.Error(w, "unavailable", http.StatusInternalServerError)
		}
	}))
	defer docker.Close()
	cli, err := client.NewClientWithOpts(client.WithHost(strings.Replace(docker.URL, "http://", "tcp://", 1)), client.WithVersion("1.45"))
	require.NoError(t, err)
	defer cli.Close()
	e := &Environment{Id: "test", client: cli, meta: &Metadata{}, Configuration: &environment.Configuration{}, st: system.NewAtomicString(environment.ProcessRunningState), emitter: events.NewBus(), reattachedAt: time.Now(), reattachedCount: 3}
	done := make(chan bool, 1)
	go func() { done <- e.reattach("old", 0) }()
	select {
	case <-killStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not attempt to terminate the old container")
	}
	replacement.Store(true)
	require.NoError(t, e.Start(context.Background()))
	conn := <-connections
	defer conn.Close()
	defer e.SetStream(nil)
	require.True(t, e.IsAttached())
	release.Do(func() { close(finishKill) })
	select {
	case attached := <-done:
		require.True(t, attached, "a new attachment supersedes the old recovery")
	case <-time.After(5 * time.Second):
		t.Fatal("stale recovery did not exit")
	}
	require.Zero(t, replacementKills.Load())
	require.True(t, e.IsAttached())
	require.Equal(t, environment.ProcessRunningState, e.State())
}
