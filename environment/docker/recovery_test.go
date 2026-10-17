package docker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
			go func() { done <- e.reattach() }()
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
