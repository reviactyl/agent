package filesystem

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	. "github.com/franela/goblin"
	"github.com/mholt/archives"
	"github.com/reviactyl/agent/internal/progress"
	"golang.org/x/sys/unix"
)

func TestArchiveTarPreservesSymlinks(t *testing.T) {
	for _, format := range []string{"", "tar.gz"} {
		t.Run("format="+format, func(t *testing.T) {
			for _, selection := range []string{"all", "files", "ignore", "base"} {
				t.Run(selection, func(t *testing.T) {
					fs, rfs := NewFs()
					t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
					content := strings.NewReader("tar content")
					if err := fs.Write("nested/deep/file.txt", content, content.Size(), 0o644); err != nil {
						t.Fatal(err)
					}
					links := map[string]string{
						"nested/link.txt":       "deep/file.txt",
						"nested/deep/link.txt":  "file.txt",
						"nested/deep/dangling":  "missing.txt",
						"nested/deep/directory": "..",
						"nested/deep/external":  "/etc/hostname",
					}
					for name, target := range links {
						if err := os.Symlink(target, filepath.Join(rfs.root, "server", name)); err != nil {
							t.Fatal(err)
						}
					}
					a := &Archive{Filesystem: fs, Format: format}
					switch selection {
					case "files":
						a.Files = []string{"nested"}
					case "ignore":
						a.Ignore = "nested/deep/dangling"
						delete(links, "nested/deep/dangling")
					case "base":
						a.BaseDirectory = "nested"
					}
					archivePath := filepath.Join(t.TempDir(), "archive.tar.gz")
					if err := a.Create(context.Background(), archivePath); err != nil {
						t.Fatal(err)
					}
					file, err := os.Open(archivePath)
					if err != nil {
						t.Fatal(err)
					}
					defer file.Close()
					gz, err := gzip.NewReader(file)
					if err != nil {
						t.Fatal(err)
					}
					defer gz.Close()
					reader := tar.NewReader(gz)
					entries := map[string]*tar.Header{}
					for {
						header, err := reader.Next()
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
						if _, exists := entries[header.Name]; exists {
							t.Fatalf("duplicate entry %q", header.Name)
						}
						entries[header.Name] = header
						if header.Typeflag == tar.TypeReg {
							data, err := io.ReadAll(reader)
							if err != nil || string(data) != "tar content" {
								t.Fatalf("unexpected file content %q: %v", data, err)
							}
						}
					}
					if len(entries) != len(links)+1 {
						t.Fatalf("unexpected TAR entries: %v", entries)
					}
					for name, target := range links {
						if selection == "base" {
							name = strings.TrimPrefix(name, "nested/")
						}
						header := entries[name]
						if header == nil || header.Typeflag != tar.TypeSymlink || header.Linkname != target {
							t.Fatalf("symlink %q: got %+v, want target %q", name, header, target)
						}
					}
				})
			}
		})
	}
}

func TestArchiveZip(t *testing.T) {
	fs, rfs := NewFs()
	content := strings.NewReader("zip content")
	if err := fs.Write("nested/file.txt", content, content.Size(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file.txt", filepath.Join(rfs.root, "server/nested/link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(rfs.root, "server/nested/pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "archive.zip")
	tracker := progress.NewProgress(uint64(len("zip content")))
	if err := (&Archive{Filesystem: fs, Format: "zip", Progress: tracker}).Create(context.Background(), archivePath); err != nil {
		t.Fatal(err)
	}
	if tracker.Written() != uint64(len("zip content")) {
		t.Fatalf("ZIP progress is %d bytes", tracker.Written())
	}
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	entries := map[string]bool{}
	for _, f := range r.File {
		entries[f.Name] = true
		if f.Name == "nested/link.txt" && f.Mode()&os.ModeSymlink == 0 {
			t.Fatal("ZIP symlink lost its mode")
		}
	}
	if !entries["nested/file.txt"] || !entries["nested/link.txt"] {
		t.Fatalf("missing ZIP entries: %v", entries)
	}
	if entries["nested/pipe"] {
		t.Fatal("FIFO must not be included in ZIP")
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	var linkTarget string
	if err := (archives.Zip{}).Extract(context.Background(), archive, func(_ context.Context, f archives.FileInfo) error {
		if f.NameInArchive != "nested/link.txt" {
			return nil
		}
		if f.Mode()&os.ModeSymlink == 0 {
			t.Fatal("ZIP extraction lost symlink mode")
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		linkTarget = string(data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if linkTarget != "file.txt" {
		t.Fatalf("ZIP symlink target is %q", linkTarget)
	}
}

func TestArchive_Stream(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("Archive", func() {
		g.AfterEach(func() {
			// Reset the filesystem after each run.
			_ = fs.TruncateRootDirectory()
		})

		g.It("creates archive with intended files", func() {
			g.Assert(fs.CreateDirectory("test", "/")).IsNil()
			g.Assert(fs.CreateDirectory("test2", "/")).IsNil()

			r := strings.NewReader("hello, world!\n")
			err := fs.Write("test/file.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			r = strings.NewReader("hello, world!\n")
			err = fs.Write("test2/file.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			r = strings.NewReader("hello, world!\n")
			err = fs.Write("test_file.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			r = strings.NewReader("hello, world!\n")
			err = fs.Write("test_file.txt.old", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			a := &Archive{
				Filesystem: fs,
				Files: []string{
					"test",
					"test_file.txt",
				},
			}

			// Create the archive.
			archivePath := filepath.Join(rfs.root, "archive.tar.gz")
			g.Assert(a.Create(context.Background(), archivePath)).IsNil()

			// Ensure the archive exists.
			_, err = os.Stat(archivePath)
			g.Assert(err).IsNil()

			// Open the archive.
			genericFs, err := archives.FileSystem(context.Background(), archivePath, nil)
			g.Assert(err).IsNil()

			// Assert that we are opening an archive.
			afs, ok := genericFs.(iofs.ReadDirFS)
			g.Assert(ok).IsTrue()

			// Get the names of the files recursively from the archive.
			files, err := getFiles(afs, ".")
			g.Assert(err).IsNil()

			// Ensure the files in the archive match what we are expecting.
			expected := []string{
				"test_file.txt",
				"test/file.txt",
			}

			// Sort the slices to ensure the comparison never fails if the
			// contents are sorted differently.
			sort.Strings(expected)
			sort.Strings(files)

			g.Assert(files).Equal(expected)
		})
	})
}

func getFiles(f iofs.ReadDirFS, name string) ([]string, error) {
	var v []string

	entries, err := f.ReadDir(name)
	if err != nil {
		return nil, err
	}

	for _, e := range entries {
		entryName := e.Name()
		if name != "." {
			entryName = filepath.Join(name, entryName)
		}

		if e.IsDir() {
			files, err := getFiles(f, entryName)
			if err != nil {
				return nil, err
			}

			if files == nil {
				return nil, nil
			}

			v = append(v, files...)
			continue
		}

		v = append(v, entryName)
	}

	return v, nil
}
