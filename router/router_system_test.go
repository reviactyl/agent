package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/reviactyl/agent/config"
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
