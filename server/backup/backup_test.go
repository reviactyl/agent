package backup

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/server/filesystem"
)

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
