package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	dockerclient "github.com/docker/docker/client"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/environment"
	"github.com/reviactyl/agent/remote"
	"github.com/reviactyl/agent/router/middleware"
	"github.com/reviactyl/agent/server"
	"github.com/reviactyl/agent/system"
	"gopkg.in/yaml.v2"
)

func TestGetSystemInformationIncludesInstallationType(t *testing.T) {
	original := readSystemInformation
	t.Cleanup(func() { readSystemInformation = original })
	readSystemInformation = func() (*system.Information, error) {
		return &system.Information{
			Version:          "26.09.1",
			InstallationType: "native",
			System: system.System{
				Architecture:  "amd64",
				CPUThreads:    4,
				KernelVersion: "6.8.0",
				OSType:        "linux",
			},
		}, nil
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/system", nil)
	getSystemInformation(c)

	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["installation_type"] != "native" {
		t.Fatalf("unexpected installation type: %#v", response)
	}
}

func TestPostSystemUpdateRejectsDockerInstallations(t *testing.T) {
	originalType := system.InstallationType
	originalInstall := installSystemUpdate
	t.Cleanup(func() {
		system.InstallationType = originalType
		installSystemUpdate = originalInstall
	})
	systemUpdateInProgress.Store(false)
	t.Cleanup(func() { systemUpdateInProgress.Store(false) })
	system.InstallationType = "docker"
	called := false
	installSystemUpdate = func(context.Context, string, string) (*system.InstalledUpdate, error) {
		called = true
		return nil, errors.New("should not be called")
	}

	recorder := httptest.NewRecorder()
	engine := gin.New()
	engine.Use(middleware.CaptureErrors())
	engine.POST("/api/system/update", postSystemUpdate)
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/system/update", strings.NewReader(`{"version":"26.09.1"}`))
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected conflict, got %d", recorder.Code)
	}
	if called {
		t.Fatal("Docker update invoked the native updater")
	}
}

func TestPostSystemUpdateRejectsInvalidRequests(t *testing.T) {
	originalType := system.InstallationType
	originalInstall := installSystemUpdate
	t.Cleanup(func() {
		system.InstallationType = originalType
		installSystemUpdate = originalInstall
		systemUpdateInProgress.Store(false)
	})
	system.InstallationType = "native"
	installSystemUpdate = func(context.Context, string, string) (*system.InstalledUpdate, error) {
		t.Fatal("invalid request invoked the updater")
		return nil, nil
	}

	for name, body := range map[string]string{
		"missing version": `{}`,
		"invalid channel": `{"version":"26.10.0-beta.1","channel":"nightly"}`,
		"malformed JSON":  `{"version":`,
	} {
		t.Run(name, func(t *testing.T) {
			systemUpdateInProgress.Store(false)
			recorder := httptest.NewRecorder()
			engine := gin.New()
			engine.Use(middleware.CaptureErrors())
			engine.POST("/api/system/update", postSystemUpdate)
			request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/system/update", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("expected bad request, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if systemUpdateInProgress.Load() {
				t.Fatal("update lock remained set after invalid request")
			}
		})
	}
}

func TestPostSystemUpdateInstallsBeforeSchedulingRestart(t *testing.T) {
	originalType := system.InstallationType
	originalInstall := installSystemUpdate
	originalRestart := restartAfterSystemUpdate
	originalScheduleLockRelease := scheduleSystemUpdateLockRelease
	t.Cleanup(func() {
		system.InstallationType = originalType
		installSystemUpdate = originalInstall
		restartAfterSystemUpdate = originalRestart
		scheduleSystemUpdateLockRelease = originalScheduleLockRelease
	})
	systemUpdateInProgress.Store(false)
	t.Cleanup(func() { systemUpdateInProgress.Store(false) })
	system.InstallationType = "native"
	var releaseLock func()
	scheduleSystemUpdateLockRelease = func(delay time.Duration, callback func()) *time.Timer {
		if delay != systemUpdateLockRelease {
			t.Fatalf("unexpected update lock release delay: %s", delay)
		}
		releaseLock = callback

		return nil
	}
	installed := &system.InstalledUpdate{ExecutablePath: "/agent", BackupPath: "/agent.update-backup"}
	installSystemUpdate = func(ctx context.Context, version string, channel string) (*system.InstalledUpdate, error) {
		if channel != "beta" {
			t.Fatalf("unexpected channel %q", channel)
		}
		if version != "26.10.0-rc.1" {
			t.Fatalf("unexpected version %q", version)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > systemUpdateTimeout {
			t.Fatalf("unexpected update deadline: %v, %v", deadline, ok)
		}
		return installed, nil
	}
	restarted := make(chan *system.InstalledUpdate, 1)
	restartAfterSystemUpdate = func(ctx context.Context, update *system.InstalledUpdate) (bool, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > systemUpdateRestartTimeout {
			t.Fatalf("unexpected restart deadline: %v", deadline)
		}
		restarted <- update
		return true, nil
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/system/update", strings.NewReader(`{"version":"26.10.0-rc.1","channel":"beta"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	postSystemUpdate(c)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected accepted response, got %d: %s", recorder.Code, recorder.Body.String())
	}
	select {
	case update := <-restarted:
		if update != installed {
			t.Fatalf("unexpected installed update: %#v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("expected restart to be scheduled")
	}
	if !systemUpdateInProgress.Load() {
		t.Fatal("update lock was released before the restart grace period")
	}
	if releaseLock == nil {
		t.Fatal("update lock release was not scheduled")
	}
	releaseLock()
	if systemUpdateInProgress.Load() {
		t.Fatal("update lock remained set after the restart grace period")
	}
}

func TestPostSystemUpdateRejectsConcurrentAttempts(t *testing.T) {
	originalType := system.InstallationType
	t.Cleanup(func() {
		system.InstallationType = originalType
		systemUpdateInProgress.Store(false)
	})
	system.InstallationType = "native"
	systemUpdateInProgress.Store(true)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/system/update", strings.NewReader(`{"version":"26.09.1"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	postSystemUpdate(c)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected conflict, got %d", recorder.Code)
	}
}

func TestPostSystemUpdateKeepsLockDuringActiveOperation(t *testing.T) {
	originalType := system.InstallationType
	originalInstall := installSystemUpdate
	originalRestart := restartAfterSystemUpdate
	t.Cleanup(func() {
		system.InstallationType = originalType
		installSystemUpdate = originalInstall
		restartAfterSystemUpdate = originalRestart
		systemUpdateInProgress.Store(false)
	})
	system.InstallationType = "native"

	for _, phase := range []string{"install", "restart"} {
		t.Run(phase, func(t *testing.T) {
			systemUpdateInProgress.Store(false)
			entered := make(chan struct{})
			release := make(chan struct{})
			finished := make(chan struct{})
			defer func() {
				close(release)
				<-finished
			}()
			installSystemUpdate = func(context.Context, string, string) (*system.InstalledUpdate, error) {
				if phase == "install" {
					close(entered)
					<-release
					return nil, errors.New("install failed")
				}
				return nil, nil
			}
			restartAfterSystemUpdate = func(context.Context, *system.InstalledUpdate) (bool, error) {
				close(entered)
				<-release
				return true, errors.New("restart failed")
			}
			engine := gin.New()
			engine.Use(middleware.CaptureErrors())
			engine.POST("/api/system/update", postSystemUpdate)
			requestUpdate := func() *httptest.ResponseRecorder {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/api/system/update", strings.NewReader(`{"version":"26.09.1"}`))
				request.Header.Set("Content-Type", "application/json")
				engine.ServeHTTP(recorder, request)
				return recorder
			}
			go func() {
				defer close(finished)
				requestUpdate()
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("update did not enter blocked operation")
			}
			if response := requestUpdate(); response.Code != http.StatusConflict {
				t.Fatalf("concurrent update during %s returned %d, expected conflict", phase, response.Code)
			}
		})
	}
}

func TestPostSystemUpdateRollsBackWhenRestartCannotBeScheduled(t *testing.T) {
	originalType := system.InstallationType
	originalInstall := installSystemUpdate
	originalRestart := restartAfterSystemUpdate
	t.Cleanup(func() {
		system.InstallationType = originalType
		installSystemUpdate = originalInstall
		restartAfterSystemUpdate = originalRestart
		systemUpdateInProgress.Store(false)
	})
	systemUpdateInProgress.Store(false)
	system.InstallationType = "native"
	directory := t.TempDir()
	executable := filepath.Join(directory, "agent")
	backup := executable + ".update-backup"
	if err := os.WriteFile(executable, []byte("new agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("old agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	installSystemUpdate = func(context.Context, string, string) (*system.InstalledUpdate, error) {
		return &system.InstalledUpdate{ExecutablePath: executable, BackupPath: backup}, nil
	}
	restartAfterSystemUpdate = func(context.Context, *system.InstalledUpdate) (bool, error) {
		return true, errors.New("systemd unavailable")
	}

	recorder := httptest.NewRecorder()
	engine := gin.New()
	engine.Use(middleware.CaptureErrors())
	engine.POST("/api/system/update", postSystemUpdate)
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/system/update", strings.NewReader(`{"version":"26.09.1"}`))
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected internal server error, got %d", recorder.Code)
	}
	current, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "old agent" {
		t.Fatalf("expected old Agent binary to be restored, got %q", current)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("expected recovery backup to be consumed, got %v", err)
	}
	if systemUpdateInProgress.Load() {
		t.Fatal("update lock remained set after rollback")
	}
}

func TestPostSystemUpdateDoesNotRollBackWhenSupervisorCleanupFails(t *testing.T) {
	originalType := system.InstallationType
	originalInstall := installSystemUpdate
	originalRestart := restartAfterSystemUpdate
	originalSchedule := scheduleSystemUpdateLockRelease
	t.Cleanup(func() {
		system.InstallationType = originalType
		installSystemUpdate = originalInstall
		restartAfterSystemUpdate = originalRestart
		scheduleSystemUpdateLockRelease = originalSchedule
		systemUpdateInProgress.Store(false)
		failedSystemUpdate.Store(nil)
	})
	systemUpdateInProgress.Store(false)
	system.InstallationType = "native"
	var releaseLock func()
	scheduleSystemUpdateLockRelease = func(delay time.Duration, callback func()) *time.Timer {
		if delay != systemUpdateLockRelease {
			t.Fatalf("unexpected lock release delay: %s", delay)
		}
		releaseLock = callback
		return nil
	}
	directory := t.TempDir()
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(directory, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "agent")
	backup := executable + ".update-backup"
	if err := os.WriteFile(executable, []byte("new agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("old agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	installCalls := 0
	installSystemUpdate = func(context.Context, string, string) (*system.InstalledUpdate, error) {
		installCalls++
		return &system.InstalledUpdate{ExecutablePath: executable, BackupPath: backup}, nil
	}
	restartAfterSystemUpdate = func(context.Context, *system.InstalledUpdate) (bool, error) {
		return false, errors.New("systemd unavailable and supervisor cleanup failed")
	}
	recorder := httptest.NewRecorder()
	engine := gin.New()
	engine.Use(middleware.CaptureErrors())
	engine.POST("/api/system/update", postSystemUpdate)
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/system/update", strings.NewReader(`{"version":"26.09.1"}`))
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected internal server error, got %d", recorder.Code)
	}
	current, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "new agent" {
		t.Fatalf("expected new Agent binary to remain installed, got %q", current)
	}
	if previous, err := os.ReadFile(backup); err != nil || string(previous) != "old agent" {
		t.Fatalf("expected recovery backup to remain available, got %q, %v", previous, err)
	}
	if !systemUpdateInProgress.Load() {
		t.Fatal("update lock was released while the supervisor state remained unknown")
	}
	requestUpdate := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/system/update", strings.NewReader(`{"version":"26.09.1"}`))
		request.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(response, request)
		return response
	}
	if response := requestUpdate(); response.Code != http.StatusConflict || installCalls != 1 {
		t.Fatalf("update was not excluded during restart grace period: status %d, installs %d", response.Code, installCalls)
	}
	if releaseLock == nil {
		t.Fatal("unsafe restart failure did not schedule update lock expiry")
	}
	releaseLock()
	if systemUpdateInProgress.Load() {
		t.Fatal("update lock remained set after restart grace period")
	}
	restartAfterSystemUpdate = func(context.Context, *system.InstalledUpdate) (bool, error) {
		return true, nil
	}
	if response := requestUpdate(); response.Code != http.StatusAccepted || installCalls != 2 {
		t.Fatalf("later update was not accepted: status %d, installs %d", response.Code, installCalls)
	}
	t.Log("unsafe failure: 500, grace-period retry: 409, expiry releases guard, later retry: 202; installed binary and backup preserved")
}

func TestPostSystemUpdateTimesOutBlockedRestartAndReleasesLock(t *testing.T) {
	originalType := system.InstallationType
	originalInstall := installSystemUpdate
	originalRestart := restartAfterSystemUpdate
	originalRestartTimeout := systemUpdateRestartTimeout
	t.Cleanup(func() {
		system.InstallationType = originalType
		installSystemUpdate = originalInstall
		restartAfterSystemUpdate = originalRestart
		systemUpdateRestartTimeout = originalRestartTimeout
		systemUpdateInProgress.Store(false)
	})
	systemUpdateInProgress.Store(false)
	system.InstallationType = "native"
	systemUpdateRestartTimeout = 50 * time.Millisecond

	directory := t.TempDir()
	executable := filepath.Join(directory, "agent")
	backup := executable + ".update-backup"
	if err := os.WriteFile(executable, []byte("new agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("old agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	installSystemUpdate = func(ctx context.Context, _ string, _ string) (*system.InstalledUpdate, error) {
		if err := ctx.Err(); err != nil {
			t.Fatalf("install context inherited request cancellation: %v", err)
		}
		return &system.InstalledUpdate{ExecutablePath: executable, BackupPath: backup}, nil
	}
	restartAfterSystemUpdate = func(ctx context.Context, _ *system.InstalledUpdate) (bool, error) {
		<-ctx.Done()
		return true, ctx.Err()
	}

	requestContext, cancel := context.WithCancel(context.Background())
	cancel()
	defer cancel()
	recorder := httptest.NewRecorder()
	engine := gin.New()
	engine.Use(middleware.CaptureErrors())
	engine.POST("/api/system/update", postSystemUpdate)
	request := httptest.NewRequestWithContext(requestContext, http.MethodPost, "/api/system/update", strings.NewReader(`{"version":"26.09.1"}`))
	request.Header.Set("Content-Type", "application/json")
	started := time.Now()
	engine.ServeHTTP(recorder, request)

	if elapsed := time.Since(started); elapsed < systemUpdateRestartTimeout || elapsed >= time.Second {
		t.Fatalf("blocked restart did not use its detached timeout: %s", elapsed)
	}
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected internal server error, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if systemUpdateInProgress.Load() {
		t.Fatal("update lock remained set after restart timeout")
	}
	current, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "old agent" {
		t.Fatalf("expected old Agent binary to be restored, got %q", current)
	}
}

func TestPostUpdateConfigurationRotatesCredentials(t *testing.T) {
	t.Setenv("AGENT_TOKEN_ID", "")
	t.Setenv("AGENT_TOKEN", "")

	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthenticationTokenId = "old-id"
	cfg.AuthenticationToken = "old-token"
	if err := cfg.ResolveToken(false); err != nil {
		t.Fatal(err)
	}
	config.Set(cfg)

	credentials := make(chan [2]string, 1)
	manager := server.NewEmptyManager(backupTestRemoteClient{credentials: credentials})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("manager", manager)
	c.Request = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/update", strings.NewReader(`{"token_id":"new-id","token":"new-token"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	postUpdateConfiguration(c)

	if recorder.Code != 200 {
		t.Fatalf("expected successful update, got status %d", recorder.Code)
	}
	updated := config.Get()
	if updated.Token.ID != "new-id" || updated.Token.Token != "new-token" {
		t.Fatalf("unexpected resolved credentials: %#v", updated.Token)
	}
	select {
	case got := <-credentials:
		if got != [2]string{"new-id", "new-token"} {
			t.Fatalf("unexpected client credentials: %#v", got)
		}
	default:
		t.Fatal("expected client credentials to be rotated")
	}
}

func TestPostSystemUpdateReturnsBadRequestForInvalidVersion(t *testing.T) {
	originalType := system.InstallationType
	t.Cleanup(func() { system.InstallationType = originalType; systemUpdateInProgress.Store(false) })
	system.InstallationType = "native"
	systemUpdateInProgress.Store(false)
	recorder := httptest.NewRecorder()
	engine := gin.New()
	engine.Use(middleware.CaptureErrors())
	engine.POST("/api/system/update", postSystemUpdate)
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/system/update", strings.NewReader(`{"version":"26.10.0/../../agent","channel":"beta"}`))
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if systemUpdateInProgress.Load() {
		t.Fatal("invalid version retained the update lock")
	}
}
func TestPostCreateServerRejectsExistingServer(t *testing.T) {
	const uuid = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	s, err := server.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(map[string]string{"uuid": uuid})
	if err := s.SyncWithConfiguration(remote.ServerConfigurationResponse{Settings: settings, ProcessConfiguration: &remote.ProcessConfiguration{}}); err != nil {
		t.Fatal(err)
	}
	manager := server.NewEmptyManager(backupTestRemoteClient{})
	manager.Add(s)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("manager", manager)
	c.Request = httptest.NewRequest("POST", "/api/servers", strings.NewReader(`{"uuid":"`+uuid+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	postCreateServer(c)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected a conflict, got status %d", recorder.Code)
	}
	if manager.Len() != 1 {
		t.Fatalf("expected one server, got %d", manager.Len())
	}
}

func TestPostUpdateConfigurationOnlyAppliesPanelValues(t *testing.T) {
	t.Setenv("AGENT_TOKEN_ID", "")
	t.Setenv("AGENT_TOKEN", "")

	p := filepath.Join(t.TempDir(), "config.yml")
	cfg, err := config.NewAtPath(p)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthenticationTokenId = "old-id"
	cfg.AuthenticationToken = "old-token"
	cfg.System.Username = "pterodactyl"
	cfg.System.User.Uid = 988
	cfg.System.User.Gid = 988
	if err := cfg.ResolveToken(false); err != nil {
		t.Fatal(err)
	}
	config.Set(cfg)

	// Everything the Panel sends, plus values that only the node operator may set.
	body := `{
		"debug": true,
		"uuid": "node-uuid",
		"token_id": "new-id",
		"token": "new-token",
		"api": {
			"host": "127.0.0.1",
			"port": 8443,
			"ssl": {"enabled": true, "cert": "/srv/certs/node.pem", "key": "/srv/certs/node.key"},
			"upload_limit": 250,
			"trusted_proxies": ["10.0.0.0/8"]
		},
		"system": {
			"data": "/srv/data",
			"root_directory": "/srv/root",
			"sftp": {"bind_port": 2023, "bind_address": "127.0.0.2", "read_only": true},
			"user": {"uid": 1000, "gid": 1000, "rootless": {"enabled": true}},
			"username": "other",
			"passwd": {"enabled": true, "directory": "/srv/passwd"},
			"machineid": {"enabled": true, "directory": "/srv/machine-id"},
			"openatmode": "openat",
			"backups": {"restore_host_allowlist": ["10.0.0.0/8"]}
		},
		"docker": {
			"network": {"mode": "bridge"},
			"userns_mode": "private",
			"registries": {"registry.example.com": {"username": "u", "password": "p"}}
		},
		"allowed_mounts": ["/srv"],
		"allowed_origins": ["https://other.invalid"],
		"remote": "http://other.invalid",
		"ignore_panel_config_updates": false
	}`

	expected := config.Get()
	expected.Debug = true
	expected.Uuid = "node-uuid"
	expected.AuthenticationTokenId = "new-id"
	expected.AuthenticationToken = "new-token"
	expected.Api.Host = "127.0.0.1"
	expected.Api.Port = 8443
	expected.Api.Ssl.Enabled = true
	expected.Api.UploadLimit = 250
	expected.System.Sftp.Port = 2023
	expected.AllowedOrigins = []string{"https://other.invalid"}
	want, err := yaml.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}

	manager := server.NewEmptyManager(backupTestRemoteClient{credentials: make(chan [2]string, 1)})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("manager", manager)
	c.Request = httptest.NewRequest("POST", "/api/update", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	postUpdateConfiguration(c)

	if recorder.Code != 200 {
		t.Fatalf("expected successful update, got status %d", recorder.Code)
	}
	got, err := yaml.Marshal(config.Get())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("unexpected configuration after update:\n%s\nexpected:\n%s", got, want)
	}
	onDisk, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(want) {
		t.Fatalf("unexpected configuration written to disk:\n%s", onDisk)
	}
}

type createServerTestClient struct {
	remote.Client
	configuration func() remote.ServerConfigurationResponse
	status        func(context.Context, remote.InstallStatusRequest) error
}

func (c createServerTestClient) GetServerConfiguration(context.Context, string) (remote.ServerConfigurationResponse, error) {
	return c.configuration(), nil
}

func (c createServerTestClient) GetInstallationScript(context.Context, string) (remote.InstallationScript, error) {
	return remote.InstallationScript{}, errors.New("installation script unavailable")
}

func (c createServerTestClient) SetInstallationStatus(ctx context.Context, _ string, status remote.InstallStatusRequest) error {
	return c.status(ctx, status)
}

func TestPostCreateServerBackgroundInstallation(t *testing.T) {
	for _, test := range []struct {
		name        string
		failure     string
		statusError bool
		replacement bool
		recovery    string
	}{
		{name: "environment failure", failure: "environment"},
		{name: "installation failure", failure: "install"},
		{name: "environment failure with notification failure", failure: "environment", statusError: true},
		{name: "installation failure with notification failure", failure: "install", statusError: true},
		{name: "environment failure preserves replacement", failure: "environment", replacement: true},
		{name: "installation failure preserves replacement", failure: "install", replacement: true},
		{name: "environment failure allows reinstall", failure: "environment", recovery: "reinstall"},
		{name: "installation failure allows reinstall", failure: "install", recovery: "reinstall"},
		{name: "environment failure allows deletion", failure: "environment", recovery: "delete"},
		{name: "installation failure allows deletion", failure: "install", recovery: "delete"},
		{name: "successful installation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			const uuid = "ab012345-6789-4abc-8def-0123456789ab"
			cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
			require.NoError(t, err)
			cfg.AuthenticationToken = "test-token"
			require.NoError(t, cfg.ResolveToken(false))
			cfg.System.Data = t.TempDir()
			cfg.System.MachineID.Enable = false
			config.Set(cfg)

			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce, inspectOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			var retry atomic.Bool
			dockerAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/_ping") {
					w.Header().Set("API-Version", "1.45")
					w.WriteHeader(http.StatusOK)
					return
				}
				if r.Method == http.MethodDelete && test.recovery == "delete" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/containers/"+uuid+"/json") {
					http.Error(w, "unexpected Docker request: "+r.URL.Path, http.StatusBadRequest)
					return
				}
				inspectOnce.Do(func() {
					close(entered)
					<-release
				})
				if test.failure == "environment" && !retry.Load() {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"message":"environment creation unavailable"}`))
					return
				}
				_, _ = w.Write([]byte(`{"Id":"` + uuid + `","State":{"Running":false}}`))
			}))
			t.Cleanup(dockerAPI.Close)
			t.Cleanup(unblock)
			cli, err := environment.Docker()
			require.NoError(t, err)
			originalHTTPClient := cli.HTTPClient()
			require.NoError(t, dockerclient.WithHTTPClient(&http.Client{Transport: createServerTestTransport(func(r *http.Request) (*http.Response, error) {
				r = r.Clone(r.Context())
				r.URL.Scheme = "http"
				r.URL.Host = dockerAPI.Listener.Addr().String()
				return dockerAPI.Client().Do(r)
			})})(cli))
			t.Cleanup(func() { require.NoError(t, dockerclient.WithHTTPClient(originalHTTPClient)(cli)) })

			statuses := make(chan remote.InstallStatusRequest, 2)
			client := createServerTestClient{
				configuration: func() remote.ServerConfigurationResponse {
					settings, _ := json.Marshal(map[string]any{"uuid": uuid, "skip_egg_scripts": test.failure != "install" || retry.Load()})
					return remote.ServerConfigurationResponse{Settings: settings, ProcessConfiguration: &remote.ProcessConfiguration{}}
				},
				status: func(ctx context.Context, status remote.InstallStatusRequest) error {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					statuses <- status
					if test.statusError {
						return errors.New("panel notification unavailable")
					}
					return nil
				},
			}
			manager := server.NewEmptyManager(client)
			create := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Set("manager", manager)
				c.Request = httptest.NewRequest(http.MethodPost, "/api/servers", strings.NewReader(`{"uuid":"`+uuid+`"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				postCreateServer(c)
				c.Writer.WriteHeaderNow()
				return w
			}
			require.Equal(t, http.StatusAccepted, create().Code)
			s, ok := manager.Get(uuid)
			require.True(t, ok)
			t.Cleanup(s.CleanupForDestroy)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("background environment creation did not start")
			}
			sftpContext := s.Sftp().Context("test")
			websocketContext, websocketCancel := context.WithCancel(context.Background())
			s.Websockets().Push([16]byte{1}, &websocketCancel)
			logPath := filepath.Join(s.Filesystem().Path(), "install-diagnostic.log")
			require.NoError(t, os.WriteFile(logPath, []byte("recoverable installation data"), 0o600))

			var replacement *server.Server
			if test.replacement {
				replacement, err = server.New(client)
				require.NoError(t, err)
				require.NoError(t, replacement.SyncWithConfiguration(client.configuration()))
				t.Cleanup(replacement.CleanupForDestroy)
				manager.Remove(func(match *server.Server) bool { return match == s })
				require.True(t, manager.AddIfMissing(replacement))
			}
			unblock()
			select {
			case status := <-statuses:
				require.Equal(t, test.failure == "", status.Successful)
				require.False(t, status.Reinstall)
			case <-time.After(time.Second):
				t.Fatal("background installation did not report its outcome to the Panel")
			}

			if test.failure == "" {
				require.Same(t, s, manager.Find(func(match *server.Server) bool { return match.ID() == uuid }))
				require.NoError(t, s.Context().Err())
				require.NoError(t, sftpContext.Err())
				require.Equal(t, http.StatusConflict, create().Code)
				return
			}
			require.Eventually(t, func() bool {
				current, exists := manager.Get(uuid)
				return s.Context().Err() != nil && websocketContext.Err() != nil && sftpContext.Err() != nil && ((!exists && replacement == nil) || (current == replacement && replacement != nil))
			}, time.Second, time.Millisecond, "failed original instance must be canceled and unregistered without removing a replacement")
			data, err := os.ReadFile(logPath)
			require.NoError(t, err)
			require.Equal(t, "recoverable installation data", string(data))
			if replacement != nil {
				require.NoError(t, replacement.Context().Err())
				return
			}

			retry.Store(true)
			if test.recovery != "" {
				engine := Configure(manager, client)
				path := "/api/servers/" + uuid
				method := http.MethodDelete
				if test.recovery == "reinstall" {
					method = http.MethodPost
					path += "/reinstall"
				}
				request := func(method, path, token string) *httptest.ResponseRecorder {
					req := httptest.NewRequest(method, path, nil)
					if token != "" {
						req.Header.Set("Authorization", "Bearer "+token)
					}
					w := httptest.NewRecorder()
					engine.ServeHTTP(w, req)
					return w
				}
				require.Equal(t, http.StatusUnauthorized, request(method, path, "").Code)
				require.Equal(t, http.StatusForbidden, request(method, path, "wrong-token").Code)
				require.Equal(t, http.StatusNotFound, request(http.MethodGet, "/api/servers/"+uuid, "test-token").Code)
				_, exists := manager.Get(uuid)
				require.False(t, exists, "normal and unauthorized requests must not recover failed registrations")
				response := request(method, path, "test-token")
				if test.recovery == "delete" {
					require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
					_, exists = manager.Get(uuid)
					require.False(t, exists)
					require.Eventually(t, func() bool { _, err := os.Stat(filepath.Dir(logPath)); return os.IsNotExist(err) }, time.Second, time.Millisecond)
					return
				}
				require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
				recovered, exists := manager.Get(uuid)
				require.True(t, exists)
				t.Cleanup(recovered.CleanupForDestroy)
				require.NotSame(t, s, recovered)
				require.NoError(t, recovered.Context().Err())
				select {
				case status := <-statuses:
					require.True(t, status.Successful)
					require.True(t, status.Reinstall)
				case <-time.After(time.Second):
					t.Fatal("failed installation could not be reinstalled")
				}
				require.Equal(t, http.StatusConflict, create().Code)
				return
			}
			require.Equal(t, http.StatusAccepted, create().Code, "retry must not conflict with the failed instance")
			retried, ok := manager.Get(uuid)
			require.True(t, ok)
			require.NotSame(t, s, retried)
			t.Cleanup(retried.CleanupForDestroy)
			select {
			case status := <-statuses:
				require.True(t, status.Successful)
			case <-time.After(time.Second):
				t.Fatal("retry did not complete successfully")
			}
			require.Same(t, retried, manager.Find(func(match *server.Server) bool { return match.ID() == uuid }))
			require.NoError(t, retried.Context().Err())
		})
	}
}

type createServerTestTransport func(*http.Request) (*http.Response, error)

func (f createServerTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestPostUpdateConfigurationUpdatesCORS(t *testing.T) {
	t.Setenv("AGENT_TOKEN_ID", "")
	t.Setenv("AGENT_TOKEN", "")
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg, err := config.NewAtPath(path)
	require.NoError(t, err)
	cfg.AuthenticationTokenId = "test-id"
	cfg.AuthenticationToken = "test-token"
	cfg.PanelLocation = "https://panel.test"
	cfg.AllowedOrigins = []string{"https://old.test"}
	require.NoError(t, cfg.ResolveToken(false))
	config.Set(cfg)
	require.NoError(t, config.WriteToDisk(cfg))
	client := backupTestRemoteClient{}
	engine := Configure(server.NewEmptyManager(client), client)

	request := func(method, uri, body, token, origin string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, uri, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}
	checkOrigins := func(t *testing.T, allowed []string) {
		t.Helper()
		require.Equal(t, allowed, config.Get().AllowedOrigins)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var persisted config.Configuration
		require.NoError(t, yaml.Unmarshal(data, &persisted))
		require.ElementsMatch(t, allowed, persisted.AllowedOrigins)
		for _, origin := range []string{cfg.PanelLocation, "https://old.test", "https://new.test", "https://untrusted.test"} {
			want := cfg.PanelLocation
			for _, permitted := range allowed {
				if origin != cfg.PanelLocation && (origin == permitted || permitted == "*") {
					want = permitted
					break
				}
			}
			for _, method := range []string{http.MethodGet, http.MethodOptions} {
				rec := request(method, "/api/servers", "", "test-token", origin)
				status := http.StatusOK
				if method == http.MethodOptions {
					status = http.StatusNoContent
				}
				require.Equal(t, status, rec.Code, rec.Body.String())
				require.Equal(t, want, rec.Header().Get("Access-Control-Allow-Origin"), "%s %s", method, origin)
				require.Equal(t, "true", rec.Header().Get("Access-Control-Allow-Credentials"))
			}
		}
	}
	checkOrigins(t, []string{"https://old.test"})
	for _, token := range []string{"", "invalid-token"} {
		rec := request(http.MethodPost, "/api/update", `{"allowed_origins":["https://new.test"]}`, token, cfg.PanelLocation)
		require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code)
		checkOrigins(t, []string{"https://old.test"})
	}

	for _, step := range []struct {
		name    string
		body    string
		status  int
		origins []string
	}{
		{"replace", `{"allowed_origins":["https://new.test"]}`, http.StatusOK, []string{"https://new.test"}},
		{"omitted", `{"debug":true}`, http.StatusOK, []string{"https://new.test"}},
		{"invalid credentials", `{"allowed_origins":["https://rejected.test"],"token":"file:///invalid"}`, http.StatusInternalServerError, []string{"https://new.test"}},
		{"malformed origins", `{"allowed_origins":["https://rejected.test",42]}`, http.StatusBadRequest, []string{"https://new.test"}},
		{"remove", `{"allowed_origins":[]}`, http.StatusOK, []string{}},
		{"wildcard", `{"allowed_origins":["*"]}`, http.StatusOK, []string{"*"}},
		{"null clears", `{"allowed_origins":null}`, http.StatusOK, nil},
	} {
		t.Run(step.name, func(t *testing.T) {
			rec := request(http.MethodPost, "/api/update", step.body, "test-token", cfg.PanelLocation)
			require.Equal(t, step.status, rec.Code, rec.Body.String())
			if step.status == http.StatusOK {
				require.JSONEq(t, `{"applied":true}`, rec.Body.String())
			}
			checkOrigins(t, step.origins)
		})
	}

	t.Run("concurrent preflight", func(t *testing.T) {
		done := make(chan struct{})
		result := make(chan string, 1)
		go func() {
			for {
				select {
				case <-done:
					result <- ""
					return
				default:
					rec := request(http.MethodOptions, "/api/servers", "", "", "https://new.test")
					origin := rec.Header().Get("Access-Control-Allow-Origin")
					if rec.Code != http.StatusNoContent || (origin != cfg.PanelLocation && origin != "https://new.test") {
						result <- "unexpected preflight response: " + origin
						return
					}
				}
			}
		}()
		defer func() {
			close(done)
			require.Empty(t, <-result)
		}()
		for i := 0; i < 20; i++ {
			origin := "https://new.test"
			if i%2 == 0 {
				origin = "https://old.test"
			}
			rec := request(http.MethodPost, "/api/update", `{"allowed_origins":["`+origin+`"]}`, "test-token", cfg.PanelLocation)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}
	})
	checkOrigins(t, []string{"https://new.test"})

	config.Update(func(c *config.Configuration) { c.IgnorePanelConfigUpdates = true })
	rec := request(http.MethodPost, "/api/update", `{"allowed_origins":["https://old.test"]}`, "test-token", cfg.PanelLocation)
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"applied":false}`, rec.Body.String())
	checkOrigins(t, []string{"https://new.test"})
}

func TestUnsafeUpdateRetryRecoversBackupBeforeRealInstall(t *testing.T) {
	originalType, originalInstall, originalRestart, originalSchedule := system.InstallationType, installSystemUpdate, restartAfterSystemUpdate, scheduleSystemUpdateLockRelease
	t.Cleanup(func() {
		system.InstallationType, installSystemUpdate, restartAfterSystemUpdate, scheduleSystemUpdateLockRelease = originalType, originalInstall, originalRestart, originalSchedule
		systemUpdateInProgress.Store(false)
		failedSystemUpdate.Store(nil)
	})
	system.InstallationType = "native"
	systemUpdateInProgress.Store(false)
	directory := t.TempDir()
	executable := filepath.Join(directory, "agent")
	backup := executable + ".update-backup"
	if err := os.WriteFile(executable, []byte("old agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	binary := "#!/bin/sh\nprintf 'agent v26.09.1\\n'\n"
	digest := sha256.Sum256([]byte(binary))
	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/metadata/") {
			fmt.Fprintf(w, `{"tag_name":"v26.09.1","assets":[{"name":"agent_linux_amd64","digest":"sha256:%s"}]}`, hex.EncodeToString(digest[:]))
		} else {
			fmt.Fprint(w, binary)
		}
	}))
	defer releases.Close()
	updater := system.NewUpdater()
	updater.ReleaseBaseURL, updater.ReleaseMetadataURL = releases.URL, releases.URL+"/metadata"
	updater.ExecutablePath = func() (string, error) { return executable, nil }
	updater.GOOS, updater.GOARCH = "linux", "amd64"
	installSystemUpdate = updater.Install
	allowStop := filepath.Join(directory, "allow-stop")
	t.Setenv("REV137_STOP_ALLOWED", allowStop)
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := "#!/bin/sh\nif [ \"$1\" = show ]; then echo loaded; exit 0; fi\n[ -f \"$REV137_STOP_ALLOWED\" ]\n"
	if err := os.WriteFile(filepath.Join(directory, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var release func()
	scheduleSystemUpdateLockRelease = func(_ time.Duration, callback func()) *time.Timer { release = callback; return nil }
	restartAfterSystemUpdate = func(context.Context, *system.InstalledUpdate) (bool, error) {
		return false, errors.New("supervisor uncertain")
	}
	engine := gin.New()
	engine.Use(middleware.CaptureErrors())
	engine.POST("/api/system/update", postSystemUpdate)
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/system/update", strings.NewReader(`{"version":"26.09.1"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		return w
	}
	checkFiles := func() {
		t.Helper()
		for path, want := range map[string]string{executable: binary, backup: "old agent"} {
			got, err := os.ReadFile(path)
			if err != nil || string(got) != want {
				t.Fatalf("changed recovery files at %s: %q, %v", path, got, err)
			}
		}
	}
	if r := request(); r.Code != http.StatusInternalServerError {
		t.Fatalf("initial update: %d %s", r.Code, r.Body.String())
	}
	checkFiles()
	if r := request(); r.Code != http.StatusConflict {
		t.Fatalf("active supervisor not excluded: %d", r.Code)
	}
	if release == nil {
		t.Fatal("missing expiry")
	}
	release()
	if r := request(); r.Code != http.StatusInternalServerError {
		t.Fatalf("uncertain supervisor retry: %d", r.Code)
	}
	checkFiles()
	if err := os.WriteFile(allowStop, []byte("yes"), 0o600); err != nil {
		t.Fatal(err)
	}
	restartAfterSystemUpdate = func(context.Context, *system.InstalledUpdate) (bool, error) { return true, nil }
	if r := request(); r.Code != http.StatusAccepted {
		t.Fatalf("real updater retry failed: %d %s", r.Code, r.Body.String())
	}
	checkFiles()
}
