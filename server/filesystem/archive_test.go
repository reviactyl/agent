package filesystem

import (
	"archive/zip"
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
