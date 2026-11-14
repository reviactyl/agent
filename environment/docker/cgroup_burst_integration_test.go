package docker

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/environment"
	"github.com/stretchr/testify/require"
)

func TestInSituUpdateCpuBurst(t *testing.T) {
	networkName := os.Getenv("REVIACTYL_TEST_CPU_BURST_NETWORK")
	if networkName == "" {
		t.Skip("requires an isolated Docker network and writable CPU burst cgroup")
	}
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cli.Close()) })
	cfg, err := config.NewAtPath(t.TempDir() + "/config.yml")
	require.NoError(t, err)
	cfg.AuthenticationToken = "test-token"
	cfg.Docker.UsePerformantInspect = false
	cfg.Docker.CpuBurst.Enabled = true
	cfg.Docker.CpuBurst.Percent = 50
	config.Set(cfg)
	created, err := cli.ContainerCreate(ctx, &container.Config{Image: "alpine:3.22", Cmd: []string{"sleep", "300"}}, &container.HostConfig{NetworkMode: container.NetworkMode(networkName), Resources: container.Resources{CPUPeriod: 100000, CPUQuota: 100000}}, &network.NetworkingConfig{}, nil, networkName+"-cpu-burst-test")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})) })
	require.NoError(t, cli.ContainerStart(ctx, created.ID, container.StartOptions{}))
	inspected, err := cli.ContainerInspect(ctx, created.ID)
	require.NoError(t, err)
	proc, err := os.ReadFile("/proc/" + strconv.Itoa(inspected.State.Pid) + "/cgroup")
	require.NoError(t, err)
	burstPath, err := resolveCgroupCpuFile(string(proc), cgroupV2())
	require.NoError(t, err)
	readBurst := func() string {
		b, err := os.ReadFile(burstPath)
		require.NoError(t, err)
		return strings.TrimSpace(string(b))
	}
	quotaPath := filepath.Join(filepath.Dir(burstPath), "cpu.cfs_quota_us")
	if cgroupV2() {
		quotaPath = filepath.Join(filepath.Dir(burstPath), "cpu.max")
	}
	readQuota := func() string {
		b, err := os.ReadFile(quotaPath)
		require.NoError(t, err)
		return strings.Fields(string(b))[0]
	}
	e := &Environment{Id: created.ID, client: cli}
	for _, enabled := range []bool{true, false} {
		t.Run("rejected_enabled_"+strconv.FormatBool(enabled), func(t *testing.T) {
			cfg.Docker.CpuBurst.Enabled = enabled
			config.Set(cfg)
			require.NoError(t, os.WriteFile(burstPath, []byte("40000"), 0644))
			e.Configuration = environment.NewConfiguration(environment.Settings{Limits: environment.Limits{CpuLimit: 25, MemoryLimit: 64, Threads: "999999"}}, nil)
			err := e.InSituUpdate()
			require.ErrorContains(t, err, "could not update container")
			require.ErrorContains(t, err, "cpuset cpus")
			current, inspectErr := cli.ContainerInspect(ctx, created.ID)
			require.NoError(t, inspectErr)
			require.EqualValues(t, 100000, current.HostConfig.CPUQuota)
			require.Equal(t, "100000", readQuota())
			t.Logf("Docker rejected update: %v; quota=%d burst=%s", err, current.HostConfig.CPUQuota, readBurst())
			require.Equal(t, "40000", readBurst())
		})
	}
	t.Run("successful_lower_quota", func(t *testing.T) {
		cfg.Docker.CpuBurst.Enabled = true
		config.Set(cfg)
		require.NoError(t, os.WriteFile(burstPath, []byte("90000"), 0644))
		e.Configuration = environment.NewConfiguration(environment.Settings{Limits: environment.Limits{CpuLimit: 25, MemoryLimit: 64}}, nil)
		require.NoError(t, e.InSituUpdate())
		current, err := cli.ContainerInspect(ctx, created.ID)
		require.NoError(t, err)
		require.EqualValues(t, 25000, current.HostConfig.CPUQuota)
		require.Equal(t, "25000", readQuota())
		require.Equal(t, "12500", readBurst())
		t.Logf("Docker accepted lower quota=%d; burst=%s", current.HostConfig.CPUQuota, readBurst())
	})
}
