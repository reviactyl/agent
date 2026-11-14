package docker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/environment"
	"github.com/stretchr/testify/require"
)

func TestCpuBurstMicroseconds(t *testing.T) {
	tests := []struct {
		name     string
		quota    int64
		percent  int64
		expected int64
	}{
		{name: "full quota", quota: 200_000, percent: 100, expected: 200_000},
		{name: "half quota", quota: 200_000, percent: 50, expected: 100_000},
		{name: "zero percent", quota: 200_000, percent: 0, expected: 0},
		{name: "percent above kernel cap", quota: 200_000, percent: 150, expected: 200_000},
		{name: "negative percent", quota: 200_000, percent: -50, expected: 0},
		{name: "no quota", quota: 0, percent: 100, expected: 0},
		{name: "negative quota", quota: -1, percent: 100, expected: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if v := cpuBurstMicroseconds(tt.quota, tt.percent); v != tt.expected {
				t.Errorf("expected %d, got %d", tt.expected, v)
			}
		})
	}
}

func TestResolveCgroupCpuFile(t *testing.T) {
	tests := []struct {
		name       string
		procCgroup string
		v2         bool
		expected   string
		wantErr    bool
	}{
		{
			name:       "v2 systemd scope",
			procCgroup: "0::/system.slice/docker-abc123.scope\n",
			v2:         true,
			expected:   "/sys/fs/cgroup/system.slice/docker-abc123.scope/cpu.max.burst",
		},
		{
			name:       "v2 rootless",
			procCgroup: "0::/user.slice/user-1000.slice/user@1000.service/user.slice/docker-abc123.scope\n",
			v2:         true,
			expected:   "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/user.slice/docker-abc123.scope/cpu.max.burst",
		},
		{
			name:       "v1 combined cpu controller",
			procCgroup: "12:pids:/docker/abc123\n4:cpu,cpuacct:/docker/abc123\n1:name=systemd:/docker/abc123\n",
			v2:         false,
			expected:   "/sys/fs/cgroup/cpu/docker/abc123/cpu.cfs_burst_us",
		},
		{
			name:       "v1 bare cpu controller",
			procCgroup: "4:cpu:/docker/abc123\n",
			v2:         false,
			expected:   "/sys/fs/cgroup/cpu/docker/abc123/cpu.cfs_burst_us",
		},
		{
			name:       "v1 cpuset and cpuacct do not match",
			procCgroup: "5:cpuset:/docker/abc123\n4:cpuacct:/docker/abc123\n",
			v2:         false,
			wantErr:    true,
		},
		{
			name:       "v1 host ignores the unified hierarchy line",
			procCgroup: "0::/docker/abc123\n",
			v2:         false,
			wantErr:    true,
		},
		{
			name:       "namespaced relative path",
			procCgroup: "0::/../../system.slice\n",
			v2:         true,
			wantErr:    true,
		},
		{
			name:       "empty content",
			procCgroup: "",
			v2:         true,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := resolveCgroupCpuFile(tt.procCgroup, tt.v2)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected an error, got %q", f)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if f != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, f)
			}
		})
	}
}

func TestClearCpuBurstRollback(t *testing.T) {
	for _, previous := range []string{"0", "40000\n"} {
		t.Run(previous, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "cpu.max.burst")
			require.NoError(t, os.WriteFile(f, []byte(previous), 0600))
			e := &Environment{Id: "test"}
			restore := e.clearCpuBurstFile(f)
			b, err := os.ReadFile(f)
			require.NoError(t, err)
			require.Equal(t, "0", string(b), "clear before Docker lowers quota")
			restore()
			b, err = os.ReadFile(f)
			require.NoError(t, err)
			require.Equal(t, previous, string(b), "restore the actual previous value")
		})
	}
}

func TestClearCpuBurstWithoutReadableSnapshot(t *testing.T) {
	e := &Environment{Id: "test"}
	f := filepath.Join(t.TempDir(), "missing-burst")
	restore := e.clearCpuBurstFile(f)
	restore()
	_, err := os.Stat(f)
	require.True(t, os.IsNotExist(err), "do not clear burst without a rollback value")
	e.clearCpuBurst(0)()
}

func TestCpuBurstWritersWaitForResourceUpdate(t *testing.T) {
	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	require.NoError(t, err)
	cfg.AuthenticationToken = "test-token"
	cfg.Docker.UsePerformantInspect = false
	cfg.Docker.CpuBurst.Enabled = true
	config.Set(cfg)

	for _, writer := range []string{"resource update", "burst application"} {
		t.Run(writer, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			var unblock sync.Once
			defer unblock.Do(func() { close(release) })
			var inspections atomic.Int32
			var updates atomic.Int32
			docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					inspections.Add(1)
					_, _ = io.WriteString(w, `{"Id":"server","State":{"Pid":0}}`)
					return
				}
				if updates.Add(1) == 1 {
					close(entered)
					<-release
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `{"message":"update rejected"}`)
					return
				}
				_, _ = io.WriteString(w, `{}`)
			}))
			defer docker.Close()
			defer unblock.Do(func() { close(release) })
			cli, err := client.NewClientWithOpts(client.WithHost(docker.URL), client.WithVersion("1.45"))
			require.NoError(t, err)
			defer cli.Close()
			newEnvironment := func() *Environment {
				return &Environment{Id: "server", client: cli, Configuration: environment.NewConfiguration(environment.Settings{Limits: environment.Limits{CpuLimit: 50}}, nil)}
			}
			first := make(chan error, 1)
			go func() { first <- newEnvironment().InSituUpdate() }()
			select {
			case <-entered:
			case <-time.After(time.Second * 5):
				t.Fatal("first update did not reach Docker")
			}
			second := make(chan error, 1)
			started := make(chan struct{})
			go func() {
				close(started)
				if writer == "resource update" {
					second <- newEnvironment().InSituUpdate()
				} else {
					SetCpuBurst(context.Background(), cli, "server", 50000)
					second <- nil
				}
			}()
			<-started
			select {
			case result := <-second:
				second <- result
				t.Error("another writer finished before the pending update could restore its burst")
			case <-time.After(100 * time.Millisecond):
			}
			require.EqualValues(t, 1, inspections.Load(), "other writers must wait before inspecting or changing burst")
			unblock.Do(func() { close(release) })
			require.ErrorContains(t, <-first, "update rejected")
			require.NoError(t, <-second)
			require.GreaterOrEqual(t, inspections.Load(), int32(2))
		})
	}
}

func TestCpuBurstLockCancellation(t *testing.T) {
	unlock, err := lockCpuBurst(context.Background(), "blocked")
	require.NoError(t, err)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	waiting, err := lockCpuBurst(ctx, "blocked")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, waiting)
	other, err := lockCpuBurst(context.Background(), "other")
	require.NoError(t, err)
	other()
}

func TestResourceUpdateLockDeadline(t *testing.T) {
	unlock, err := lockCpuBurst(context.Background(), "blocked-update")
	require.NoError(t, err)
	defer unlock()
	e := &Environment{Id: "blocked-update"}
	require.ErrorIs(t, e.InSituUpdate(), context.DeadlineExceeded)
}
