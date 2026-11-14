//go:build unix

package ufs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPrivateDirectoryExcludesSandboxOperations(t *testing.T) {
	for _, openat2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("openat2=%t", openat2), func(t *testing.T) {
			root := t.TempDir()
			fs, err := NewUnixFS(root, openat2)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "staging"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"staging/copy", "sentinel"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("staging", filepath.Join(root, "alias")); err != nil {
				t.Fatal(err)
			}
			dirfd, _, closeFD, err := fs.SafePath("staging/copy")
			if err != nil {
				t.Fatal(err)
			}
			defer closeFD()
			unprotect, err := fs.ProtectDirectory(dirfd)
			if err != nil {
				t.Fatal(err)
			}
			defer unprotect()
			open := func(path string, flags int) error {
				f, err := fs.OpenFile(path, flags, 0o600)
				if f != nil {
					_ = f.Close()
				}
				return err
			}
			for name, operation := range map[string]func() error{
				"read":              func() error { return open("staging/copy", O_RDONLY) },
				"write":             func() error { return open("staging/copy", O_WRONLY|O_TRUNC) },
				"create":            func() error { return open("staging/new", O_WRONLY|O_CREATE) },
				"alias read":        func() error { return open("alias/copy", O_RDONLY) },
				"rename file out":   func() error { return fs.Rename("staging/copy", "moved-copy") },
				"rename file in":    func() error { return fs.Rename("sentinel", "staging/new") },
				"replace file":      func() error { return fs.Replace("sentinel", "staging/copy") },
				"rename directory":  func() error { return fs.Rename("staging", "moved") },
				"alias destination": func() error { return fs.Rename("sentinel", "alias/new") },
				"chmod directory":   func() error { return fs.Chmod("staging", 0o777) },
				"chown directory":   func() error { return fs.Chown("staging", os.Getuid(), os.Getgid()) },
				"chown alias":       func() error { return fs.Chown("alias", os.Getuid(), os.Getgid()) },
				"lchown directory":  func() error { return fs.Lchown("staging", os.Getuid(), os.Getgid()) },
				"chtimes":           func() error { return fs.Chtimes("staging", time.Now(), time.Now()) },
				"mkdir":             func() error { return fs.Mkdir("staging/new", 0o700) },
				"symlink":           func() error { return fs.Symlink("../sentinel", "staging/new") },
				"hard link out":     func() error { return fs.Link("staging/copy", "copy-link") },
				"hard link in":      func() error { return fs.Link("sentinel", "staging/new") },
				"remove file":       func() error { return fs.Remove("staging/copy") },
				"remove tree":       func() error { return fs.RemoveAll("staging") },
				"remove contents":   func() error { return fs.RemoveContents("staging") },
				"held descriptor": func() error {
					f, err := fs.OpenFileat(dirfd, "copy", O_WRONLY|O_TRUNC, 0)
					if f != nil {
						_ = f.Close()
					}
					return err
				},
			} {
				t.Run(name, func(t *testing.T) {
					err := operation()
					if (name == "alias read" || name == "alias destination") && errors.Is(err, ErrNotDirectory) {
						return
					}
					if !errors.Is(err, os.ErrPermission) {
						t.Fatalf("private directory operation returned %v", err)
					}
				})
			}
			other, err := NewUnixFS(root, openat2)
			if err != nil {
				t.Fatal(err)
			}
			if err := other.Rename("staging/copy", "copy-through-another-sandbox"); !errors.Is(err, os.ErrPermission) {
				t.Fatalf("another sandbox accessed private staging: %v", err)
			}
			entries, err := fs.ReadDir(".")
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() == "staging" {
					t.Fatal("private directory is visible through sandbox listings")
				}
			}
			if err := os.Rename(filepath.Join(root, "staging"), filepath.Join(root, "renamed")); err != nil {
				t.Fatal(err)
			}
			if err := open("renamed/copy", O_RDONLY); !errors.Is(err, os.ErrPermission) {
				t.Fatalf("renamed private directory became accessible: %v", err)
			}
			for _, name := range []string{"renamed/copy", "sentinel"} {
				if data, err := os.ReadFile(filepath.Join(root, name)); err != nil || string(data) != "preserve" {
					t.Fatalf("private operation changed %s: %q, %v", name, data, err)
				}
			}
			unprotect()
			if err := fs.Rename("renamed/copy", "recovered-copy"); err != nil {
				t.Fatalf("released directory remains inaccessible: %v", err)
			}
			if err := unix.Fstat(dirfd, &unix.Stat_t{}); err != nil {
				t.Fatal("protecting a directory closed the caller's descriptor")
			}
		})
	}
}
