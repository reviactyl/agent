package sftp

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/apex/log"
	pkgsftp "github.com/pkg/sftp"

	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/internal/database"
	"github.com/reviactyl/agent/server"
	"github.com/reviactyl/agent/server/filesystem"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "sftp-tests-")
	if err != nil {
		panic(err)
	}
	config.Set(&config.Configuration{AuthenticationToken: "test", System: config.SystemConfiguration{RootDirectory: root, DiskCheckInterval: 60}})
	if err := database.Initialize(); err != nil {
		_ = os.RemoveAll(root)
		panic(err)
	}
	code := m.Run()
	if db, err := database.Instance().DB(); err == nil {
		_ = db.Close()
	}
	_ = os.RemoveAll(root)
	os.Exit(code)
}

func TestHandlerFilewriteTruncatesOnlyWhenRequested(t *testing.T) {
	for _, tt := range []struct {
		name   string
		flags  uint32
		offset int64
		want   string
	}{
		{name: "resume", flags: 2, offset: 6, want: "ABCDEFGHI"},
		{name: "overwrite", flags: 2, offset: 2, want: "ABGHIF"},
		{name: "create and resume", flags: 2 | 8, offset: 6, want: "ABCDEFGHI"},
		{name: "truncate", flags: 2 | 16, want: "GHI"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "upload.txt")
			if err := os.WriteFile(path, []byte("ABCDEF"), 0o644); err != nil {
				t.Fatal(err)
			}
			fs, err := filesystem.New(root, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = fs.UnixFS().Close() })
			srv, err := server.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			h := Handler{server: srv, fs: fs, events: &eventHandler{}, permissions: []string{"*"}, logger: log.WithField("test", tt.name)}
			request := pkgsftp.NewRequest("Put", "/upload.txt")
			request.Flags = tt.flags
			writer, err := h.Filewrite(request)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = writer.(io.Closer).Close() })
			if n, err := writer.WriteAt([]byte("GHI"), tt.offset); err != nil || n != 3 {
				t.Fatalf("WriteAt = %d, %v", n, err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("contents = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandlerFilewriteReplacementQuota(t *testing.T) {
	for _, tt := range []struct {
		name        string
		limit       int64
		exists      bool
		flags       uint32
		data        string
		offset      int64
		want        string
		openDenied  bool
		writeDenied bool
	}{
		{name: "over quota smaller", limit: 8, exists: true, flags: 2 | 16, data: "abc", want: "abc"},
		{name: "over quota still over after shrinking", limit: 2, exists: true, flags: 2 | 16, data: "abc", want: "abc"},
		{name: "over quota larger", limit: 8, exists: true, flags: 2 | 16, data: "abcdefghijkl", want: "", writeDenied: true},
		{name: "over quota new file", limit: 8, flags: 2 | 8 | 16, data: "abc", openDenied: true},
		{name: "over quota overwrite", limit: 8, exists: true, flags: 2, data: "abc", want: "abcDEF"},
		{name: "over quota resume grows", limit: 8, exists: true, flags: 2, data: "abc", offset: 6, want: "ABCDEF", writeDenied: true},
		{name: "under quota smaller", limit: 30, exists: true, flags: 2 | 16, data: "abc", want: "abc"},
		{name: "under quota larger", limit: 30, exists: true, flags: 2 | 16, data: "abcdefghijkl", want: "abcdefghijkl"},
		{name: "under quota new file", limit: 30, flags: 2 | 8 | 16, data: "abc", want: "abc"},
		{name: "writes disabled", limit: -1, exists: true, flags: 2 | 16, data: "abc", openDenied: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("1234567890"), 0o644); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "upload.txt")
			if tt.exists {
				if err := os.WriteFile(path, []byte("ABCDEF"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			fs, err := filesystem.New(root, tt.limit, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = fs.UnixFS().Close() })
			srv, err := server.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			h := Handler{server: srv, fs: fs, events: &eventHandler{}, permissions: []string{"*"}, logger: log.WithField("test", tt.name)}
			request := pkgsftp.NewRequest("Put", "/upload.txt")
			request.Flags = tt.flags
			writer, err := h.Filewrite(request)
			if tt.openDenied {
				if !errors.Is(err, ErrSSHQuotaExceeded) {
					t.Fatalf("open error = %v, want quota exceeded", err)
				}
				if tt.exists {
					got, err := os.ReadFile(path)
					if err != nil || string(got) != "ABCDEF" {
						t.Fatalf("denied open changed file: %q, %v", got, err)
					}
				} else if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("denied open created file: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			n, writeErr := writer.WriteAt([]byte(tt.data), tt.offset)
			if err := writer.(io.Closer).Close(); err != nil {
				t.Fatal(err)
			}
			if tt.writeDenied {
				if n != 0 || !errors.Is(writeErr, ErrSSHQuotaExceeded) {
					t.Fatalf("write = %d, %v, want quota exceeded", n, writeErr)
				}
			} else if n != len(tt.data) || writeErr != nil {
				t.Fatalf("write = %d, %v", n, writeErr)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("contents = %q, want %q", got, tt.want)
			}
			if usage := fs.CachedUsage(); usage != 10+int64(len(tt.want)) {
				t.Fatalf("cached usage = %d, want %d", usage, 10+len(tt.want))
			}
		})
	}
}

type writeAtFunc func([]byte, int64) (int, error)

func (f writeAtFunc) WriteAt(p []byte, off int64) (int, error) {
	return f(p, off)
}

func TestHandlerDeniesAccessWhenServerIsInProtectedState(t *testing.T) {
	tests := []struct {
		name string
		set  func(*server.Server)
	}{
		{
			name: "installing",
			set: func(s *server.Server) {
				s.SetInstalling(true)
			},
		},
		{
			name: "transferring",
			set: func(s *server.Server) {
				s.SetTransferring(true)
			},
		},
		{
			name: "restoring",
			set: func(s *server.Server) {
				s.SetRestoring(true)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, err := server.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			tt.set(srv)

			h := Handler{
				server:      srv,
				permissions: []string{"*"},
			}

			if h.can(PermissionFileCreate) {
				t.Fatal("expected SFTP access to be denied")
			}
		})
	}
}

func TestWriterDeniesWritesWhenServerEntersProtectedState(t *testing.T) {
	tests := []struct {
		name string
		set  func(*server.Server)
	}{
		{
			name: "installing",
			set: func(s *server.Server) {
				s.SetInstalling(true)
			},
		},
		{
			name: "transferring",
			set: func(s *server.Server) {
				s.SetTransferring(true)
			},
		},
		{
			name: "restoring",
			set: func(s *server.Server) {
				s.SetRestoring(true)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, err := server.New(nil)
			if err != nil {
				t.Fatal(err)
			}

			var called bool
			writer := quotaWriterAt{
				server: srv,
				WriterAt: writeAtFunc(func(_ []byte, _ int64) (int, error) {
					called = true
					return 1, nil
				}),
			}
			tt.set(srv)

			n, err := writer.WriteAt([]byte("x"), 0)
			if !errors.Is(err, pkgsftp.ErrSSHFxPermissionDenied) {
				t.Fatalf("expected permission denied, got %v", err)
			}
			if n != 0 {
				t.Fatalf("expected zero bytes written, got %d", n)
			}
			if called {
				t.Fatal("expected underlying writer not to be called")
			}
		})
	}
}

func TestWriterForwardsWritesWhenServerIsAvailable(t *testing.T) {
	srv, err := server.New(nil)
	if err != nil {
		t.Fatal(err)
	}

	writer := quotaWriterAt{
		server: srv,
		WriterAt: writeAtFunc(func(p []byte, _ int64) (int, error) {
			return len(p), io.EOF
		}),
	}

	n, err := writer.WriteAt([]byte("test"), 0)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected forwarded error, got %v", err)
	}
	if n != 4 {
		t.Fatalf("expected forwarded byte count, got %d", n)
	}
}

func TestHandlerRejectsMalformedSetstatAttributes(t *testing.T) {
	srv, err := server.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	h := Handler{
		server:      srv,
		permissions: []string{PermissionFileUpdate},
		logger:      log.WithField("test", t.Name()),
	}
	for _, tt := range []struct {
		name  string
		flags uint32
		attrs []byte
	}{
		{name: "missing size", flags: 1},
		{name: "missing times", flags: 8},
		{name: "truncated times", flags: 8, attrs: make([]byte, 4)},
		{name: "mode with missing times", flags: 4 | 8, attrs: []byte{0, 0, 1, 128}},
		{name: "extended attributes", flags: sftpAttributeExtended, attrs: []byte{255, 255, 255, 255}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request := pkgsftp.NewRequest("Setstat", "/")
			request.Flags, request.Attrs = tt.flags, tt.attrs
			if err := h.Filecmd(request); !errors.Is(err, pkgsftp.ErrSSHFxBadMessage) {
				t.Fatalf("expected bad message, got %v", err)
			}
		})
	}
}

func TestSetstatMode(t *testing.T) {
	tests := []struct {
		name     string
		mode     uint32
		expected uint32
	}{
		{name: "file permissions", mode: 0o600, expected: 0o600},
		{name: "default permissions", mode: 0o000, expected: 0o644},
		{name: "directory permissions", mode: 0o040700, expected: 0o755},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := pkgsftp.NewRequest("Setstat", "/test")
			request.Flags = 4 // SSH_FILEXFER_ATTR_PERMISSIONS
			request.Attrs = make([]byte, 4)
			binary.BigEndian.PutUint32(request.Attrs, tt.mode)

			mode := setstatMode(request.Attributes())
			if uint32(mode) != tt.expected {
				t.Fatalf("expected mode %04o, got %04o", tt.expected, mode)
			}
		})
	}
}

func TestHandlerSetstatUpdatesOnlyRequestedAttributes(t *testing.T) {
	for _, tt := range []struct {
		name        string
		flags       uint32
		initialMode os.FileMode
		wantMode    os.FileMode
	}{
		{name: "timestamp only executable", flags: 8, initialMode: 0o751, wantMode: 0o751},
		{name: "timestamp only special permissions", flags: 8, initialMode: 0o751 | os.ModeSetuid | os.ModeSetgid | os.ModeSticky, wantMode: 0o751 | os.ModeSetuid | os.ModeSetgid | os.ModeSticky},
		{name: "mode and timestamp", flags: 4 | 8, initialMode: 0o751, wantMode: 0o640},
		{name: "mode only", flags: 4, initialMode: 0o751, wantMode: 0o640},
		{name: "no attributes", initialMode: 0o751, wantMode: 0o751},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "run.sh")
			if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.initialMode); err != nil {
				t.Fatal(err)
			}
			oldTime := time.Unix(1600000000, 0)
			if err := os.Chtimes(path, oldTime, oldTime); err != nil {
				t.Fatal(err)
			}
			fs, err := filesystem.New(root, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = fs.UnixFS().Close() })
			srv, err := server.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			h := Handler{server: srv, fs: fs, permissions: []string{PermissionFileUpdate}, logger: log.WithField("test", t.Name())}
			request := pkgsftp.NewRequest("Setstat", "/run.sh")
			request.Flags = tt.flags
			if tt.flags&4 != 0 {
				request.Attrs = binary.BigEndian.AppendUint32(request.Attrs, 0o640)
			}
			if tt.flags&8 != 0 {
				request.Attrs = binary.BigEndian.AppendUint32(request.Attrs, 1700000000)
				request.Attrs = binary.BigEndian.AppendUint32(request.Attrs, 1700000100)
			}
			if err := h.Filecmd(request); err != nil && !errors.Is(err, pkgsftp.ErrSSHFxOk) {
				t.Fatal(err)
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode() != tt.wantMode {
				t.Errorf("mode = %v, want %v", st.Mode(), tt.wantMode)
			}
			var stat unix.Stat_t
			if err := unix.Stat(path, &stat); err != nil {
				t.Fatal(err)
			}
			wantAtime, wantMtime := oldTime.Unix(), oldTime.Unix()
			if tt.flags&8 != 0 {
				wantAtime, wantMtime = 1700000000, 1700000100
			}
			if stat.Atim.Sec != wantAtime || stat.Mtim.Sec != wantMtime {
				t.Errorf("atime/mtime = %d/%d, want %d/%d", stat.Atim.Sec, stat.Mtim.Sec, wantAtime, wantMtime)
			}
			request.Filepath = "/missing.sh"
			if err := h.Filecmd(request); !errors.Is(err, pkgsftp.ErrSSHFxNoSuchFile) {
				t.Fatalf("missing file: expected no such file, got %v", err)
			}
		})
	}
}
