package backup

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/remote"
	"github.com/reviactyl/agent/server/filesystem"
)

type backupRemoteClient struct {
	remote.Client
	getURLs func(context.Context, string, int64) (remote.BackupRemoteUploadResponse, error)
}

func (c backupRemoteClient) GetBackupRemoteUploadURLs(ctx context.Context, backup string, size int64) (remote.BackupRemoteUploadResponse, error) {
	return c.getURLs(ctx, backup, size)
}

func TestS3BackupGenerateRemoteRequestCancelsBlockedURLAcquisition(t *testing.T) {
	backupDir := t.TempDir()
	config.Set(&config.Configuration{AuthenticationToken: "test", System: config.SystemConfiguration{BackupDirectory: backupDir}})
	const uuid = "00000000-0000-0000-0000-000000000001"
	if err := os.WriteFile(filepath.Join(backupDir, uuid+".tar.gz"), []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}

	urlRequestStarted := make(chan struct{})
	client := backupRemoteClient{getURLs: func(ctx context.Context, _ string, _ int64) (remote.BackupRemoteUploadResponse, error) {
		close(urlRequestStarted)
		<-ctx.Done()
		return remote.BackupRemoteUploadResponse{}, ctx.Err()
	}}
	backup := NewS3(client, uuid, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := backup.generateRemoteRequest(ctx, io.NopCloser(strings.NewReader("backup")))
		result <- err
	}()

	select {
	case <-urlRequestStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for presigned URL request")
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("presigned URL request did not stop after cancellation")
	}
}

func TestS3BackupGenerateRemoteRequestPreservesSuccessfulUpload(t *testing.T) {
	backupDir := t.TempDir()
	config.Set(&config.Configuration{AuthenticationToken: "test", System: config.SystemConfiguration{BackupDirectory: backupDir}})
	const uuid = "00000000-0000-0000-0000-000000000001"
	data := []byte("backup")
	if err := os.WriteFile(filepath.Join(backupDir, uuid+".tar.gz"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got, err := io.ReadAll(r.Body); err != nil {
			t.Errorf("read upload: %v", err)
		} else if string(got) != string(data) {
			t.Errorf("upload body = %q, want %q", got, data)
		}
		w.Header().Set("ETag", "etag")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := backupRemoteClient{getURLs: func(context.Context, string, int64) (remote.BackupRemoteUploadResponse, error) {
		return remote.BackupRemoteUploadResponse{Parts: []string{server.URL}, PartSize: int64(len(data))}, nil
	}}
	backup := NewS3(client, uuid, "")
	file, err := os.Open(backup.Path())
	if err != nil {
		t.Fatal(err)
	}
	parts, err := backup.generateRemoteRequest(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].ETag != "etag" || parts[0].PartNumber != 1 {
		t.Fatalf("uploaded parts = %#v", parts)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("started %d upload parts, want 1", got)
	}
}

func TestS3BackupGenerateRemoteRequestUploadsPartsAndStopsAfterCancellation(t *testing.T) {
	backupDir := t.TempDir()
	config.Set(&config.Configuration{AuthenticationToken: "test", System: config.SystemConfiguration{BackupDirectory: backupDir}})
	const uuid = "00000000-0000-0000-0000-000000000001"
	data := []byte("abcdefgh")
	if err := os.WriteFile(filepath.Join(backupDir, uuid+".tar.gz"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	firstPartReceived := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		part := requests.Add(1)
		if got, err := io.ReadAll(r.Body); err != nil {
			t.Errorf("read part %d: %v", part, err)
		} else if want := string(data[(part-1)*4 : part*4]); string(got) != want {
			t.Errorf("part %d body = %q, want %q", part, got, want)
		}
		if part == 1 {
			close(firstPartReceived)
			<-ctx.Done()
		}
		w.Header().Set("ETag", "etag")
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		cancel()
		server.Close()
	}()

	client := backupRemoteClient{getURLs: func(context.Context, string, int64) (remote.BackupRemoteUploadResponse, error) {
		return remote.BackupRemoteUploadResponse{Parts: []string{server.URL + "/1", server.URL + "/2"}, PartSize: 4}, nil
	}}
	backup := NewS3(client, uuid, "")
	file, err := os.Open(backup.Path())
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := backup.generateRemoteRequest(ctx, file)
		result <- err
	}()
	select {
	case <-firstPartReceived:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first upload part")
	}

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("upload did not stop after cancellation")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("started %d upload parts after cancellation, want 1", got)
	}
}

func TestTarBackupRestorePreservesSymlinkTarget(t *testing.T) {
	config.Set(&config.Configuration{AuthenticationToken: "test"})
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "link.txt", Typeflag: tar.TypeSymlink, Linkname: "file.txt", Mode: 0o777}); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	b := NewS3(nil, "00000000-0000-0000-0000-000000000001", "")
	var target string
	if err := b.Restore(context.Background(), bytes.NewReader(archive.Bytes()), func(_ string, _ fs.FileInfo, linkTarget string, _ io.ReadCloser) error {
		target = linkTarget
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if target != "file.txt" {
		t.Fatalf("tar symlink target is %q", target)
	}
}

func TestZipBackupRestore(t *testing.T) {
	configuration := &config.Configuration{AuthenticationToken: "test", System: config.SystemConfiguration{BackupDirectory: t.TempDir()}}
	configuration.System.Backups.WriteLimit = 1
	config.Set(configuration)
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	file, err := writer.Create("nested/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("restored")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	b := NewS3(nil, "00000000-0000-0000-0000-000000000001", "", "zip")
	var name, content string
	err = b.Restore(context.Background(), io.NopCloser(bytes.NewReader(archive.Bytes())), func(file string, _ fs.FileInfo, _ string, reader io.ReadCloser) error {
		name = file
		data, err := io.ReadAll(reader)
		content = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if name != "nested/file.txt" || content != "restored" {
		t.Fatalf("restored %q with %q", name, content)
	}
	local := NewLocal(nil, "00000000-0000-0000-0000-000000000001", "", "zip")
	if err := os.WriteFile(local.Path(), archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(NewLocal(nil, local.Uuid, "").Path(), []byte("old tar archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	selected, _, err := LocateLocal(nil, local.Uuid, "zip")
	if err != nil || selected.Path() != local.Path() {
		t.Fatalf("explicit ZIP lookup selected %v: %v", selected, err)
	}
	content = ""
	if err := local.Restore(context.Background(), nil, func(_ string, _ fs.FileInfo, _ string, reader io.ReadCloser) error {
		data, err := io.ReadAll(reader)
		content = string(data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if content != "restored" {
		t.Fatalf("local ZIP restore produced %q", content)
	}
}

func TestBackupGenerateRequiresUuidIdentifier(t *testing.T) {
	tests := map[string]func(string) BackupInterface{
		"local": func(identifier string) BackupInterface {
			return NewLocal(nil, identifier, "")
		},
		"s3": func(identifier string) BackupInterface {
			return NewS3(nil, identifier, "")
		},
	}

	for name, createBackup := range tests {
		t.Run(name, func(t *testing.T) {
			testBackupGenerateRequiresUuidIdentifier(t, createBackup)
		})
	}
}

func TestBackupPathUsesBackupDirectory(t *testing.T) {
	backupDir := t.TempDir()
	config.Set(&config.Configuration{
		AuthenticationToken: "test-token",
		System: config.SystemConfiguration{
			BackupDirectory: backupDir,
		},
	})

	for _, identifier := range []string{
		"11111111-1111-1111-1111-111111111111",
		"../target/archive",
		"nested/archive",
	} {
		b := NewLocal(nil, identifier, "")
		rel, err := filepath.Rel(backupDir, b.Path())
		if err != nil {
			t.Fatal(err)
		}
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			t.Fatalf("expected backup path %q to remain under %q", b.Path(), backupDir)
		}
	}
}

func testBackupGenerateRequiresUuidIdentifier(t *testing.T, createBackup func(string) BackupInterface) {
	t.Helper()

	root := t.TempDir()
	backupDir := filepath.Join(root, "backups")
	targetDir := filepath.Join(root, "target")
	serverDir := filepath.Join(root, "server")
	for _, dir := range []string{backupDir, targetDir, serverDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config.Set(&config.Configuration{
		AuthenticationToken: "test-token",
		System: config.SystemConfiguration{
			BackupDirectory: backupDir,
		},
	})

	if err := os.WriteFile(filepath.Join(serverDir, "file.txt"), []byte("server data"), 0o600); err != nil {
		t.Fatal(err)
	}
	fsys, err := filesystem.New(serverDir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	existingArchive := filepath.Join(targetDir, "archive.tar.gz")
	existingArchiveContents := []byte("existing archive")
	if err := os.WriteFile(existingArchive, existingArchiveContents, 0o600); err != nil {
		t.Fatal(err)
	}

	b := createBackup("../target/archive")
	if _, err := b.Generate(context.Background(), fsys, ""); err == nil {
		t.Fatal("expected invalid backup identifier to be rejected")
	}

	got, err := os.ReadFile(existingArchive)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, existingArchiveContents) {
		return
	}
	t.Fatal("expected backup generation not to overwrite existing archive")
}

func TestBackupRoundTripEmptyDirectories(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			root := t.TempDir()
			serverDir := filepath.Join(root, "server")
			if err := os.MkdirAll(filepath.Join(serverDir, "empty/deep"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(serverDir, "file.txt"), []byte("backup content"), 0o644); err != nil {
				t.Fatal(err)
			}
			config.Set(&config.Configuration{AuthenticationToken: "test", System: config.SystemConfiguration{BackupDirectory: t.TempDir()}})
			fsys, err := filesystem.New(serverDir, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			local := NewLocal(nil, "00000000-0000-0000-0000-000000000001", "", format)
			if err := (&filesystem.Archive{Filesystem: fsys, Format: format}).Create(context.Background(), local.Path()); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(local.Path())
			if err != nil {
				t.Fatal(err)
			}
			for _, adapter := range []string{"local", "s3"} {
				t.Run(adapter, func(t *testing.T) {
					restored := t.TempDir()
					callback := func(name string, info fs.FileInfo, _ string, reader io.ReadCloser) error {
						dst := filepath.Join(restored, name)
						if info.IsDir() {
							return os.MkdirAll(dst, info.Mode().Perm())
						}
						contents, err := io.ReadAll(reader)
						if err != nil {
							return err
						}
						return os.WriteFile(dst, contents, info.Mode().Perm())
					}
					var err error
					if adapter == "local" {
						err = local.Restore(context.Background(), nil, callback)
					} else {
						err = NewS3(nil, local.Uuid, "", format).Restore(context.Background(), bytes.NewReader(data), callback)
					}
					if err != nil {
						t.Fatal(err)
					}
					info, err := os.Stat(filepath.Join(restored, "empty/deep"))
					if err != nil || !info.IsDir() {
						t.Fatalf("empty directory was not restored: %v", err)
					}
					content, err := os.ReadFile(filepath.Join(restored, "file.txt"))
					if err != nil || string(content) != "backup content" {
						t.Fatalf("restored file: %q, %v", content, err)
					}
				})
			}
		})
	}
}
