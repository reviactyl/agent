package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/environment"
	"github.com/reviactyl/agent/remote"
	"github.com/reviactyl/agent/server/backup"
	"github.com/reviactyl/agent/server/filesystem"
)

type backupStatusClient struct {
	remote.Client
	err           error
	request       remote.BackupRequest
	archivePath   string
	archiveExists bool
}

type callbackTestBackup struct {
	*backup.LocalBackup
}

func (b callbackTestBackup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignored string) (*backup.ArchiveDetails, error) {
	if err := (&filesystem.Archive{Filesystem: fsys, Format: b.Format, Ignore: ignored}).Create(ctx, b.Path()); err != nil {
		return nil, err
	}
	checksum, err := b.Checksum()
	if err != nil {
		return nil, err
	}
	size, err := b.Size()
	if err != nil {
		return nil, err
	}
	return &backup.ArchiveDetails{Checksum: fmt.Sprintf("%x", checksum), ChecksumType: "sha1", Size: size}, nil
}

func (c *backupStatusClient) SetBackupStatus(_ context.Context, _ string, request remote.BackupRequest) error {
	c.request = request
	_, err := os.Stat(c.archivePath)
	c.archiveExists = err == nil
	return c.err
}

func TestBackupReturnsCallbackError(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			configuration := &config.Configuration{AuthenticationToken: "test", System: config.SystemConfiguration{BackupDirectory: t.TempDir()}}
			config.Set(configuration)
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "file.txt"), []byte("backup content"), 0o644); err != nil {
				t.Fatal(err)
			}
			fsys, err := filesystem.New(source, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = fsys.UnixFS().Close() })
			callbackError := errors.New("Panel callback failed")
			b := callbackTestBackup{backup.NewLocal(nil, "00000000-0000-0000-0000-000000000001", "", format)}
			client := &backupStatusClient{err: callbackError, archivePath: b.Path()}
			srv := &Server{ctx: context.Background(), client: client, fs: fsys}
			err = srv.Backup(b)
			if !client.archiveExists || !client.request.Successful || client.request.Size == 0 || client.request.Checksum == "" {
				t.Fatalf("callback did not follow successful generation: %+v", client.request)
			}
			if _, statErr := os.Stat(b.Path()); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("archive was not removed: %v", statErr)
			}
			if !errors.Is(err, callbackError) {
				t.Fatalf("Backup returned %v, want callback error %v", err, callbackError)
			}
		})
	}
}

func TestEnvironmentVariablesTimezone(t *testing.T) {
	config.Set(&config.Configuration{
		AuthenticationToken: "abc",
		System:              config.SystemConfiguration{Timezone: "Europe/London"},
	})

	tests := []struct {
		name string
		vars environment.Variables
		want string
	}{
		{name: "defaults to the system timezone", vars: environment.Variables{}, want: "TZ=Europe/London"},
		{name: "uses the server timezone", vars: environment.Variables{"TZ": "America/New_York"}, want: "TZ=America/New_York"},
		{name: "ignores an empty server timezone", vars: environment.Variables{"TZ": ""}, want: "TZ=Europe/London"},
		{name: "ignores the local timezone", vars: environment.Variables{"TZ": "Local"}, want: "TZ=Europe/London"},
		{name: "skips an empty key of another case", vars: environment.Variables{"TZ": "America/New_York", "tz": ""}, want: "TZ=America/New_York"},
		{name: "prefers the uppercase key", vars: environment.Variables{"TZ": "America/New_York", "tz": "Asia/Tokyo"}, want: "TZ=America/New_York"},
		{name: "ignores an unknown server timezone", vars: environment.Variables{"TZ": "Not/AZone"}, want: "TZ=Europe/London"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			s.cfg.EnvVars = tt.vars

			var got []string
			for _, v := range s.GetEnvironmentVariables() {
				if len(v) > 3 && v[:3] == "TZ=" {
					got = append(got, v)
				}
			}

			if !slices.Equal(got, []string{tt.want}) {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

type offlineRestoreEnvironment struct {
	environment.ProcessEnvironment
}

func (offlineRestoreEnvironment) State() string { return environment.ProcessOfflineState }

type restoreStatusClient struct {
	remote.Client
	successful bool
}

func (c *restoreStatusClient) SendRestorationStatus(_ context.Context, _ string, successful bool) error {
	c.successful = successful
	return nil
}

func TestRestoreBackupPreservesDirectoryMetadata(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			configuration := &config.Configuration{AuthenticationToken: "test", System: config.SystemConfiguration{BackupDirectory: t.TempDir()}}
			configuration.System.User.Uid = os.Getuid()
			configuration.System.User.Gid = os.Getgid()
			config.Set(configuration)
			source := t.TempDir()
			directories := []string{"readonly", "readonly/nested", "readonly/nested/empty"}
			for _, name := range directories {
				if err := os.MkdirAll(filepath.Join(source, name), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				for _, name := range directories {
					_ = os.Chmod(filepath.Join(source, name), 0o755)
				}
			})
			if err := os.WriteFile(filepath.Join(source, "readonly/nested/file.txt"), []byte("restored content"), 0o644); err != nil {
				t.Fatal(err)
			}
			modified := time.Unix(1700000000, 0)
			for _, name := range directories {
				if err := os.Chmod(filepath.Join(source, name), 0o555); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(filepath.Join(source, name), modified, modified); err != nil {
					t.Fatal(err)
				}
			}
			sourceFS, err := filesystem.New(source, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sourceFS.UnixFS().Close() })
			local := backup.NewLocal(nil, "00000000-0000-0000-0000-000000000001", "", format)
			if err := (&filesystem.Archive{Filesystem: sourceFS, Format: format}).Create(context.Background(), local.Path()); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(local.Path())
			if err != nil {
				t.Fatal(err)
			}
			for _, adapter := range []string{"local", "s3"} {
				t.Run(adapter, func(t *testing.T) {
					destination := t.TempDir()
					t.Cleanup(func() {
						for _, name := range directories {
							_ = os.Chmod(filepath.Join(destination, name), 0o755)
						}
					})
					destinationFS, err := filesystem.New(destination, 0, nil)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = destinationFS.UnixFS().Close() })
					client := &restoreStatusClient{}
					srv := &Server{ctx: context.Background(), client: client, fs: destinationFS, Environment: offlineRestoreEnvironment{}}
					var b backup.BackupInterface = local
					var reader io.ReadCloser
					if adapter == "s3" {
						b = backup.NewS3(nil, local.Uuid, "", format)
						reader = io.NopCloser(bytes.NewReader(data))
					}
					if err := srv.RestoreBackup(b, reader); err != nil {
						t.Fatal(err)
					}
					if !client.successful {
						t.Fatal("Panel was not notified of a successful restoration")
					}
					contents, err := os.ReadFile(filepath.Join(destination, "readonly/nested/file.txt"))
					if err != nil || string(contents) != "restored content" {
						t.Fatalf("restored file: %q, %v", contents, err)
					}
					for _, name := range directories {
						info, err := os.Stat(filepath.Join(destination, name))
						if err != nil {
							t.Fatal(err)
						}
						if !info.IsDir() || info.Mode().Perm() != 0o555 {
							t.Fatalf("directory %q has mode %v, want read-only directory", name, info.Mode())
						}
						if !info.ModTime().Equal(modified) {
							t.Fatalf("directory %q timestamp = %v, want %v", name, info.ModTime(), modified)
						}
					}
					entries, err := os.ReadDir(filepath.Join(destination, "readonly/nested/empty"))
					if err != nil || len(entries) != 0 {
						t.Fatalf("empty directory contents: %v, %v", entries, err)
					}
				})
			}
		})
	}
}
