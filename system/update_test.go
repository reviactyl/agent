package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdaterRejectsInvalidVersion(t *testing.T) {
	updater := NewUpdater()
	updater.GOOS = "linux"
	updater.GOARCH = "amd64"

	_, err := updater.Install(context.Background(), "26.09.0/../../agent")
	if err == nil || !strings.Contains(err.Error(), "invalid Agent release version") {
		t.Fatalf("expected invalid version error, got %v", err)
	}
}

func TestUpdaterStagesValidatesAndInstallsRelease(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "agent")
	if err := os.WriteFile(executable, []byte("old agent"), 0o755); err != nil {
		t.Fatal(err)
	}

	binary := "#!/bin/sh\nprintf 'agent v26.09.1\\nCopyright Reviactyl\\n'\n"
	server := newReleaseServer(t, "26.09.1", binary)
	defer server.Close()

	updater := NewUpdater()
	updater.ReleaseBaseURL = server.URL
	updater.ReleaseMetadataURL = server.URL + "/metadata"
	updater.ExecutablePath = func() (string, error) { return executable, nil }
	updater.GOOS = "linux"
	updater.GOARCH = "amd64"

	installed, err := updater.Install(context.Background(), "26.09.1")
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
	if !strings.Contains(string(current), "agent v26.09.1") {
		t.Fatalf("unexpected installed binary: %q", current)
	}
	backup, err := os.ReadFile(installed.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "old agent" {
		t.Fatalf("unexpected backup contents: %q", backup)
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

	_, err := updater.Install(context.Background(), "26.09.1")
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

	_, err := updater.Install(context.Background(), "26.09.1")
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

	_, err := updater.Install(context.Background(), "26.09.1")
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
			t.Fatalf("unexpected release path %q", r.URL.Path)
		}
	}))
}
