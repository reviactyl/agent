package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdaterRejectsInvalidVersion(t *testing.T) {
	updater := NewUpdater()
	updater.GOOS = "linux"
	updater.GOARCH = "amd64"

	_, err := updater.Install(context.Background(), "26.09.0/../../agent", "stable")
	if !errors.Is(err, ErrInvalidUpdateRequest) || !strings.Contains(err.Error(), "invalid Agent release version") {
		t.Fatalf("expected invalid version error, got %v", err)
	}
}

func TestUpdaterStagesValidatesAndInstallsRelease(t *testing.T) {
	for _, version := range []string{"26.09.1", "26.10.0-beta.1", "26.10.0-rc.1"} {
		t.Run(version, func(t *testing.T) {
			channel := ""
			if strings.Contains(version, "-") {
				channel = "beta"
			}
			directory := t.TempDir()
			executable := filepath.Join(directory, "agent")
			if err := os.WriteFile(executable, []byte("old agent"), 0o755); err != nil {
				t.Fatal(err)
			}

			binary := "#!/bin/sh\nprintf 'agent v" + version + "\\nCopyright Reviactyl\\n'\n"
			server := newReleaseServer(t, version, binary)
			defer server.Close()

			updater := NewUpdater()
			updater.ReleaseBaseURL = server.URL
			updater.ReleaseMetadataURL = server.URL + "/metadata"
			updater.ExecutablePath = func() (string, error) { return executable, nil }
			updater.GOOS = "linux"
			updater.GOARCH = "amd64"

			installed, err := updater.Install(context.Background(), version, channel)
			if err != nil {
				t.Fatal(err)
			}
			resolvedExecutable, err := filepath.EvalSymlinks(executable)
			if err != nil {
				t.Fatal(err)
			}
			if installed.ExecutablePath != resolvedExecutable || installed.BackupPath != resolvedExecutable+".update-backup" {
				t.Fatalf("unexpected installed update: %#v", installed)
			}

			current, err := os.ReadFile(executable)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(current), "agent v"+version) {
				t.Fatalf("unexpected installed binary: %q", current)
			}
			backup, err := os.ReadFile(installed.BackupPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(backup) != "old agent" {
				t.Fatalf("unexpected backup contents: %q", backup)
			}
		})
	}
}

func TestUpdaterDoesNotReplaceBinaryWhenValidationFails(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "agent")
	if err := os.WriteFile(executable, []byte("old agent"), 0o755); err != nil {
		t.Fatal(err)
	}

	server := newReleaseServer(t, "26.09.1", "#!/bin/sh\necho 'agent v26.09.10'\n")
	defer server.Close()

	updater := NewUpdater()
	updater.ReleaseBaseURL = server.URL
	updater.ReleaseMetadataURL = server.URL + "/metadata"
	updater.ExecutablePath = func() (string, error) { return executable, nil }
	updater.GOOS = "linux"
	updater.GOARCH = "amd64"

	_, err := updater.Install(context.Background(), "26.09.1", "stable")
	if err == nil || !strings.Contains(err.Error(), "expected version 26.09.1") {
		t.Fatalf("expected version validation error, got %v", err)
	}
	current, readErr := os.ReadFile(executable)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(current) != "old agent" {
		t.Fatalf("current executable changed after validation failure: %q", current)
	}
	if _, statErr := os.Stat(executable + ".update-backup"); !os.IsNotExist(statErr) {
		t.Fatalf("unexpected backup after validation failure: %v", statErr)
	}
}

func TestUpdaterPreservesExistingRecoveryBackup(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "agent")
	backup := executable + ".update-backup"
	if err := os.WriteFile(executable, []byte("current agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("recovery agent"), 0o755); err != nil {
		t.Fatal(err)
	}

	server := newReleaseServer(t, "26.09.1", "#!/bin/sh\necho 'agent v26.09.1'\n")
	defer server.Close()

	updater := NewUpdater()
	updater.ReleaseBaseURL = server.URL
	updater.ReleaseMetadataURL = server.URL + "/metadata"
	updater.ExecutablePath = func() (string, error) { return executable, nil }
	updater.GOOS = "linux"
	updater.GOARCH = "amd64"

	_, err := updater.Install(context.Background(), "26.09.1", "stable")
	if err == nil || !strings.Contains(err.Error(), "previous Agent update backup") {
		t.Fatalf("expected existing backup error, got %v", err)
	}
	current, readErr := os.ReadFile(executable)
	if readErr != nil {
		t.Fatal(readErr)
	}
	recovery, readErr := os.ReadFile(backup)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(current) != "current agent" || string(recovery) != "recovery agent" {
		t.Fatalf("updater changed recovery files: current=%q backup=%q", current, recovery)
	}
}

func TestRestartAfterUpdateHonorsContext(t *testing.T) {
	binDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDirectory, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDirectory, "systemd-run"), []byte("#!/bin/sh\nexec /bin/sleep 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := RestartAfterUpdate(ctx, &InstalledUpdate{ExecutablePath: "/agent", BackupPath: "/agent.update-backup"})

	if err == nil {
		t.Fatal("expected blocked systemd-run to fail when the context expired")
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("blocked systemd-run ignored context deadline: %s", elapsed)
	}
}

func TestRollbackInstalledUpdateRestoresBackup(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "agent")
	backup := executable + ".update-backup"
	if err := os.WriteFile(executable, []byte("new agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("old agent"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := RollbackInstalledUpdate(&InstalledUpdate{ExecutablePath: executable, BackupPath: backup}); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "old agent" {
		t.Fatalf("unexpected restored executable: %q", current)
	}
}

func TestUpdaterRejectsBinaryThatDoesNotMatchOfficialDigest(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "agent")
	if err := os.WriteFile(executable, []byte("old agent"), 0o755); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/metadata/") {
			fmt.Fprint(w, `{"tag_name":"v26.09.1","assets":[{"name":"agent_linux_amd64","digest":"sha256:`+strings.Repeat("0", 64)+`"}]}`)
			return
		}
		fmt.Fprint(w, "#!/bin/sh\necho 'agent v26.09.1'\n")
	}))
	defer server.Close()

	updater := NewUpdater()
	updater.ReleaseBaseURL = server.URL
	updater.ReleaseMetadataURL = server.URL + "/metadata"
	updater.ExecutablePath = func() (string, error) { return executable, nil }
	updater.GOOS = "linux"
	updater.GOARCH = "amd64"

	_, err := updater.Install(context.Background(), "26.09.1", "stable")
	if err == nil || !strings.Contains(err.Error(), "official SHA-256 digest") {
		t.Fatalf("expected digest validation error, got %v", err)
	}
	current, readErr := os.ReadFile(executable)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(current) != "old agent" {
		t.Fatalf("current executable changed after digest failure: %q", current)
	}
}

func newReleaseServer(t *testing.T, version, binary string) *httptest.Server {
	t.Helper()
	digest := sha256.Sum256([]byte(binary))

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metadata/v" + version:
			fmt.Fprintf(w, `{"tag_name":"v%s","assets":[{"name":"agent_linux_amd64","digest":"sha256:%s"}]}`, version, hex.EncodeToString(digest[:]))
		case "/v" + version + "/agent_linux_amd64":
			fmt.Fprint(w, binary)
		default:
			t.Errorf("unexpected release path %q", r.URL.Path)
			http.Error(w, "unexpected release path", http.StatusNotFound)
		}
	}))
}

func TestReleaseChannelValidation(t *testing.T) {
	for _, tc := range []struct {
		name, version, channel    string
		prerelease, draft, reject bool
	}{
		{"stable", "26.10.0", "stable", false, false, false},
		{"stable with build metadata", "26.10.0+linux-amd64", "stable", false, false, false},
		{"beta tag without flag", "26.10.0-beta.1", "stable", false, false, true},
		{"rc tag without flag", "26.10.0-rc.1", "stable", false, false, true},
		{"prerelease flag", "26.10.0", "stable", true, false, true},
		{"beta", "26.10.0-beta.1", "beta", true, false, false},
		{"rc", "26.10.0-rc.1", "beta", true, false, false},
		{"final in beta", "26.10.0", "beta", false, false, false},
		{"draft", "26.10.0", "beta", false, true, true},
		{"alpha", "26.10.0-alpha.1", "beta", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"tag_name":"v%s","prerelease":%t,"draft":%t,"assets":[{"name":"agent_linux_amd64","digest":"sha256:%s"}]}`, tc.version, tc.prerelease, tc.draft, strings.Repeat("a", 64))
			}))
			defer server.Close()
			updater := NewUpdater()
			updater.GOARCH = "amd64"
			updater.ReleaseMetadataURL = server.URL
			_, err := updater.releaseDigest(context.Background(), tc.version, tc.channel)
			if (err != nil) != tc.reject {
				t.Fatalf("unexpected validation result: %v", err)
			}
			if tc.reject && !tc.draft && !errors.Is(err, ErrInvalidUpdateRequest) {
				t.Fatalf("expected invalid request classification, got %v", err)
			}
		})
	}
}

func TestRestartAfterUpdateStopsSupervisorWhenLaunchFails(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%t", timeout), func(t *testing.T) {
			directory := t.TempDir()
			logPath := filepath.Join(directory, "stopped-unit")
			launcher := "#!/bin/sh\necho 'launcher failed after starting unit' >&2\nexit 1\n"
			if timeout {
				launcher = "#!/bin/sh\nexec /bin/sleep 2\n"
			}
			if err := os.WriteFile(filepath.Join(directory, "systemd-run"), []byte(launcher), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "systemctl"), []byte("#!/bin/sh\nprintf '%s' \"$*\" > \"$UPDATER_TEST_LOG\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("UPDATER_TEST_LOG", logPath)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			rollbackSafe, err := RestartAfterUpdate(ctx, &InstalledUpdate{ExecutablePath: "/unused/agent", BackupPath: "/unused/backup"})
			if err == nil {
				t.Fatal("expected launch failure")
			}
			if !rollbackSafe {
				t.Fatal("expected successful supervisor cleanup to allow rollback")
			}
			if !timeout && !strings.Contains(err.Error(), "launcher failed after starting unit") {
				t.Fatalf("lost original error: %v", err)
			}
			stopped, readErr := os.ReadFile(logPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(stopped) != fmt.Sprintf("stop reviactyl-agent-update-%d", os.Getpid()) {
				t.Fatalf("wrong supervisor stopped: %q", stopped)
			}
		})
	}
}

func TestRestartAfterUpdatePreventsRollbackWhenSupervisorCleanupFails(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "systemd-run"), []byte("#!/bin/sh\necho 'launcher failed after starting unit' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "systemctl"), []byte("#!/bin/sh\necho 'cleanup failed' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))

	rollbackSafe, err := RestartAfterUpdate(context.Background(), &InstalledUpdate{ExecutablePath: "/unused/agent", BackupPath: "/unused/backup"})
	if err == nil {
		t.Fatal("expected launch and cleanup failure")
	}
	if rollbackSafe {
		t.Fatal("rollback was reported safe while the supervisor could still be running")
	}
	if !strings.Contains(err.Error(), "stop supervisor") || !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("missing supervisor cleanup failure: %v", err)
	}
}
