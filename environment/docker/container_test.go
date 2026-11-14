package docker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/environment"
	"github.com/reviactyl/agent/events"
	"github.com/reviactyl/agent/system"
	"github.com/stretchr/testify/require"
)

func TestCreateRewritesLoopbackServerIPInContainerEnvironment(t *testing.T) {
	cfg, err := config.NewAtPath(t.TempDir() + "/config.yml")
	require.NoError(t, err)
	cfg.AuthenticationToken = "test-token"
	cfg.Docker.Network.Interface = "172.18.0.1"
	config.Set(cfg)

	var created container.Config
	var createErr error
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			http.Error(w, "not found", http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			createErr = json.NewDecoder(r.Body).Decode(&created)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"Id":"test"}`)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer docker.Close()

	cli, err := client.NewClientWithOpts(
		client.WithHost(strings.Replace(docker.URL, "http://", "tcp://", 1)),
		client.WithVersion("1.45"),
	)
	require.NoError(t, err)
	defer cli.Close()

	e := &Environment{
		Id:     "test",
		client: cli,
		meta:   &Metadata{Image: "~test-image"},
		Configuration: environment.NewConfiguration(environment.Settings{}, []string{
			"SERVER_IP=127.0.0.1",
			"OTHER=value",
		}),
		st:      system.NewAtomicString(environment.ProcessOfflineState),
		emitter: events.NewBus(),
	}

	require.NoError(t, e.Create())
	require.NoError(t, createErr)
	require.Equal(t, []string{"SERVER_IP=172.18.0.1", "OTHER=value"}, created.Env)
}
