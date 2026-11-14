package docker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/require"

	"github.com/reviactyl/agent/environment"
	"github.com/reviactyl/agent/events"
	"github.com/reviactyl/agent/system"
)

func TestDestroyPublishesOfflineOnlyAfterRemoval(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		failed bool
	}{
		{name: "removed", status: http.StatusNoContent},
		{name: "not found", status: http.StatusNotFound},
		{name: "daemon error", status: http.StatusInternalServerError, failed: true},
		{name: "conflict", status: http.StatusConflict, failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			duringRemoval := make(chan string, 1)
			var e *Environment
			docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodDelete, r.Method)
				require.True(t, strings.HasSuffix(r.URL.Path, "/containers/test"))
				require.Equal(t, "1", r.URL.Query().Get("force"))
				require.Equal(t, "1", r.URL.Query().Get("v"))
				duringRemoval <- e.State()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.status != http.StatusNoContent {
					_, _ = w.Write([]byte(`{"message":"removal failed"}`))
				}
			}))
			defer docker.Close()
			cli, err := client.NewClientWithOpts(client.WithHost(docker.URL), client.WithVersion("1.45"))
			require.NoError(t, err)
			defer cli.Close()
			e = &Environment{Id: "test", client: cli, st: system.NewAtomicString(environment.ProcessRunningState), emitter: events.NewBus()}
			changes := make(chan []byte, 4)
			e.Events().On(changes)
			defer e.Events().Off(changes)

			err = e.Destroy()
			require.Equal(t, environment.ProcessStoppingState, <-duringRemoval)
			want := []string{environment.ProcessStoppingState}
			if tc.failed {
				require.ErrorContains(t, err, "removal failed")
				require.False(t, cerrdefs.IsNotFound(err))
				require.Equal(t, environment.ProcessStoppingState, e.State())
			} else {
				require.NoError(t, err)
				require.Equal(t, environment.ProcessOfflineState, e.State())
				want = append(want, environment.ProcessOfflineState)
			}
			var got []string
			for len(changes) > 0 {
				event := events.MustDecode(<-changes)
				require.Equal(t, environment.StateChangeEvent, event.Topic)
				got = append(got, event.Data.(string))
			}
			require.Equal(t, want, got)
		})
	}
}
