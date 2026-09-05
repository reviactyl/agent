package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const maxAgentBinarySize = 128 << 20

var releaseVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)

type InstalledUpdate struct {
	ExecutablePath string
	BackupPath     string
}

type Updater struct {
	Client             *http.Client
	ReleaseBaseURL     string
	ReleaseMetadataURL string
	ExecutablePath     func() (string, error)
	GOOS               string
	GOARCH             string
}

func NewUpdater() *Updater {
	return &Updater{
		Client: &http.Client{
			Timeout: 2 * time.Minute,
		},
		ReleaseBaseURL:     "https://github.com/reviactyl/agent/releases/download",
		ReleaseMetadataURL: "https://api.github.com/repos/reviactyl/agent/releases/tags",
		ExecutablePath:     os.Executable,
		GOOS:               runtime.GOOS,
		GOARCH:             runtime.GOARCH,
	}
}

func (u *Updater) Install(ctx context.Context, version string) (*InstalledUpdate, error) {
	if !releaseVersionPattern.MatchString(version) {
		return nil, errors.New("invalid Agent release version")
	}
	if u.GOOS != "linux" {
		return nil, fmt.Errorf("automatic Agent updates are unsupported on %s", u.GOOS)
	}
	if u.GOARCH != "amd64" && u.GOARCH != "arm64" {
		return nil, fmt.Errorf("automatic Agent updates are unsupported on %s", u.GOARCH)
	}

	executable, err := u.ExecutablePath()
	if err != nil {
		return nil, fmt.Errorf("resolve current Agent executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve Agent executable symlinks: %w", err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(executable), ".agent-update-*")
	if err != nil {
		return nil, fmt.Errorf("create staged Agent binary: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	expectedDigest, err := u.releaseDigest(ctx, version)
	if err != nil {
		temporary.Close()
		return nil, err
	}

	url := fmt.Sprintf("%s/v%s/agent_linux_%s", strings.TrimRight(u.ReleaseBaseURL, "/"), version, u.GOARCH)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		temporary.Close()
		return nil, fmt.Errorf("create Agent release request: %w", err)
	}
	response, err := u.Client.Do(request)
	if err != nil {
		temporary.Close()
		return nil, fmt.Errorf("download Agent release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		temporary.Close()
		return nil, fmt.Errorf("download Agent release: unexpected HTTP status %d", response.StatusCode)
	}

	written, err := io.Copy(temporary, io.LimitReader(response.Body, maxAgentBinarySize+1))
	if err != nil {
		temporary.Close()
		return nil, fmt.Errorf("write staged Agent binary: %w", err)
	}
	if written == 0 || written > maxAgentBinarySize {
		temporary.Close()
		return nil, errors.New("downloaded Agent binary has an invalid size")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return nil, fmt.Errorf("sync staged Agent binary: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return nil, fmt.Errorf("close staged Agent binary: %w", err)
	}
	actualDigest, err := fileSHA256(temporaryPath)
	if err != nil {
		return nil, fmt.Errorf("hash staged Agent binary: %w", err)
	}
	if !strings.EqualFold(actualDigest, expectedDigest) {
		return nil, errors.New("downloaded Agent binary does not match its official SHA-256 digest")
	}
	if err := os.Chmod(temporaryPath, 0o755); err != nil {
		return nil, fmt.Errorf("make staged Agent binary executable: %w", err)
	}

	output, err := exec.CommandContext(ctx, temporaryPath, "version").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("validate staged Agent binary: %w", err)
	}
	versionLine, _, _ := strings.Cut(strings.TrimSpace(string(output)), "\n")
	if strings.TrimSpace(versionLine) != "agent v"+version {
		return nil, fmt.Errorf("validate staged Agent binary: expected version %s", version)
	}

	backupPath := executable + ".update-backup"
	if _, err := os.Stat(backupPath); err == nil {
		return nil, errors.New("a previous Agent update backup still exists; recover or remove it before updating")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect existing Agent backup: %w", err)
	}
	if err := os.Link(executable, backupPath); err != nil {
		return nil, fmt.Errorf("back up current Agent binary: %w", err)
	}
	if err := os.Rename(temporaryPath, executable); err != nil {
		if cleanupErr := os.Remove(backupPath); cleanupErr != nil {
			return nil, fmt.Errorf("install Agent update: %w (backup cleanup failed: %v)", err, cleanupErr)
		}
		return nil, fmt.Errorf("install Agent update: %w", err)
	}

	return &InstalledUpdate{ExecutablePath: executable, BackupPath: backupPath}, nil
}

func (u *Updater) releaseDigest(ctx context.Context, version string) (string, error) {
	url := fmt.Sprintf("%s/v%s", strings.TrimRight(u.ReleaseMetadataURL, "/"), version)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create Agent release metadata request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "Reviactyl-Agent-Updater")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := u.Client.Do(request)
	if err != nil {
		return "", fmt.Errorf("download Agent release metadata: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download Agent release metadata: unexpected HTTP status %d", response.StatusCode)
	}

	var metadata struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&metadata); err != nil {
		return "", fmt.Errorf("decode Agent release metadata: %w", err)
	}
	if metadata.TagName != "v"+version {
		return "", errors.New("Agent release metadata returned an unexpected version")
	}
	expectedAsset := "agent_linux_" + u.GOARCH
	for _, asset := range metadata.Assets {
		if asset.Name != expectedAsset {
			continue
		}
		digest, ok := strings.CutPrefix(asset.Digest, "sha256:")
		if ok && len(digest) == sha256.Size*2 {
			if _, err := hex.DecodeString(digest); err == nil {
				return strings.ToLower(digest), nil
			}
		}
	}

	return "", errors.New("official Agent release metadata does not include a SHA-256 digest")
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// RestartAfterUpdate starts a transient systemd unit that restarts Agent and
// restores the previous binary when the new service does not become active.
// The separate unit is important: a child process left in agent.service's
// cgroup would normally be killed by the restart it is meant to supervise.
func RestartAfterUpdate(update *InstalledUpdate) error {
	if update == nil {
		return errors.New("missing installed Agent update")
	}

	script := `sleep 2
if systemctl restart agent; then
    sleep 10
    if systemctl is-active --quiet agent; then
        rm -f -- "$2"
        exit 0
    fi
fi
if mv -f -- "$2" "$1"; then
    chmod 755 "$1"
    systemctl restart agent
fi`
	unit := fmt.Sprintf("reviactyl-agent-update-%d", os.Getpid())
	command := exec.Command(
		"systemd-run",
		"--quiet",
		"--collect",
		"--unit", unit,
		"/bin/sh", "-c", script,
		"agent-update", update.ExecutablePath, update.BackupPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("start Agent update supervisor: %w: %s", err, strings.TrimSpace(string(output)))
	}

	return nil
}

func RollbackInstalledUpdate(update *InstalledUpdate) error {
	if update == nil {
		return errors.New("missing installed Agent update")
	}
	if err := os.Rename(update.BackupPath, update.ExecutablePath); err != nil {
		return fmt.Errorf("restore previous Agent binary: %w", err)
	}

	return nil
}
