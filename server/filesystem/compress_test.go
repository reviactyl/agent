package filesystem

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/franela/goblin"
)

func TestFilesystem_DecompressSymlinks(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			fs, rfs := NewFs()
			t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
			content := strings.NewReader("original content")
			if err := fs.Write("source/file.txt", content, content.Size(), 0o644); err != nil {
				t.Fatal(err)
			}
			for name, target := range map[string]string{
				"link.txt": "file.txt", "dangling.txt": "missing.txt", "directory": ".", "parent.txt": "../source/file.txt",
			} {
				if err := os.Symlink(target, filepath.Join(rfs.root, "server/source", name)); err != nil {
					t.Fatal(err)
				}
			}
			archive, err := fs.CompressFiles("/", []string{"source"}, format)
			if err != nil {
				t.Fatal(err)
			}
			// Extract both over existing links and into a directory without parents.
			for _, dir := range []string{"fresh", "/"} {
				file := archive.Name()
				if dir == "fresh" {
					if err := fs.CreateDirectory(dir, "/"); err != nil {
						t.Fatal(err)
					}
					file = "../" + file
				}
				if err := fs.DecompressFile(context.Background(), dir, file); err != nil {
					t.Fatal(err)
				}
				for name, target := range map[string]string{
					"link.txt": "file.txt", "dangling.txt": "missing.txt", "directory": ".", "parent.txt": "../source/file.txt",
				} {
					p := filepath.Join(rfs.root, "server", dir, "source", name)
					got, err := os.Readlink(p)
					if err != nil || got != target {
						t.Fatalf("%s: target = %q, error = %v; want %q", p, got, err, target)
					}
				}
				got, err := os.ReadFile(filepath.Join(rfs.root, "server", dir, "source/link.txt"))
				if err != nil || string(got) != "original content" {
					t.Fatalf("linked content = %q, error = %v", got, err)
				}
			}
			// A previous extraction may have flattened the link into a file.
			if err := fs.unixFS.Remove("source/link.txt"); err != nil {
				t.Fatal(err)
			}
			old := strings.NewReader("file.txt")
			if err := fs.Write("source/link.txt", old, old.Size(), 0o644); err != nil {
				t.Fatal(err)
			}
			before := fs.CachedUsage()
			if err := fs.DecompressFile(context.Background(), "/", archive.Name()); err != nil {
				t.Fatal(err)
			}
			if got, err := os.Readlink(filepath.Join(rfs.root, "server/source/link.txt")); err != nil || got != "file.txt" {
				t.Fatalf("replacement link = %q, error = %v", got, err)
			}
			if got := fs.CachedUsage(); got != before-old.Size() {
				t.Fatalf("usage = %d, want %d", got, before-old.Size())
			}
		})
	}
}

func TestFilesystem_DecompressSymlinkEscape(t *testing.T) {
	for _, format := range []string{"tar", "zip"} {
		t.Run(format, func(t *testing.T) {
			fs, rfs := NewFs()
			t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
			outside := t.TempDir()
			var buf bytes.Buffer
			if format == "tar" {
				w := tar.NewWriter(&buf)
				if err := w.WriteHeader(&tar.Header{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777}); err != nil {
					t.Fatal(err)
				}
				if err := w.WriteHeader(&tar.Header{Name: "escape/payload", Typeflag: tar.TypeReg, Size: 4, Mode: 0o644}); err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write([]byte("evil")); err != nil {
					t.Fatal(err)
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				w := zip.NewWriter(&buf)
				h := &zip.FileHeader{Name: "escape"}
				h.SetMode(os.ModeSymlink | 0o777)
				link, err := w.CreateHeader(h)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(link, outside); err != nil {
					t.Fatal(err)
				}
				file, err := w.Create("escape/payload")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(file, "evil"); err != nil {
					t.Fatal(err)
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
			}
			name := "attack." + format
			if err := rfs.CreateServerFile(name, buf.Bytes()); err != nil {
				t.Fatal(err)
			}
			if err := fs.DecompressFile(context.Background(), "/", name); err == nil {
				t.Fatal("extraction through an external symlink succeeded")
			}
			if _, err := os.Stat(filepath.Join(outside, "payload")); !os.IsNotExist(err) {
				t.Fatalf("outside payload: %v", err)
			}
		})
	}
}

func TestFilesystem_DecompressGzipReplacesContents(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing []byte
		content  []byte
	}{
		{"shorter", bytes.Repeat([]byte("old"), 4096), []byte("new\n")},
		{"empty", []byte("old contents"), []byte{}},
		{"longer", []byte("old"), bytes.Repeat([]byte("new"), 4096)},
		{"new file", nil, []byte("new\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs, rfs := NewFs()
			t.Cleanup(func() {
				_ = fs.unixFS.Close()
				_ = os.RemoveAll(rfs.root)
			})
			if tc.existing != nil {
				if err := rfs.CreateServerFile("test.txt", tc.existing); err != nil {
					t.Fatal(err)
				}
			}
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			if _, err := writer.Write(tc.content); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := rfs.CreateServerFile("test.txt.gz", compressed.Bytes()); err != nil {
				t.Fatal(err)
			}
			// Initialize cached usage so replacement accounting is checked too.
			if _, err := fs.DiskUsage(false); err != nil {
				t.Fatal(err)
			}
			fs.SetDiskLimit(int64(compressed.Len() + max(len(tc.existing), len(tc.content)) + 1))
			if err := fs.DecompressFile(context.Background(), "/", "test.txt.gz"); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(rfs.root, "server", "test.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(content, tc.content) {
				t.Fatalf("extracted contents differ: got %d bytes, want %d", len(content), len(tc.content))
			}
			if got, want := fs.CachedUsage(), int64(compressed.Len()+len(tc.content)); got != want {
				t.Fatalf("cached usage = %d, want %d", got, want)
			}
		})
	}
}

func TestFilesystem_DecompressGzipQuotaFailurePreservesContents(t *testing.T) {
	for _, tc := range []struct {
		name      string
		existing  []byte
		content   []byte
		otherSize int
		limit     int64
	}{
		{"growth", []byte("old contents"), bytes.Repeat([]byte("new"), 4096), 0, 0},
		{"over quota", bytes.Repeat([]byte("o"), 5000), []byte("new\n"), 15000, 10000},
		{"replacement exceeds remaining quota", bytes.Repeat([]byte("o"), 5000), bytes.Repeat([]byte("n"), 4096), 9000, 10000},
		{"empty writes disabled", []byte("old contents"), []byte{}, 0, -1},
		{"writes disabled", []byte("old contents"), []byte("new\n"), 0, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs, rfs := NewFs()
			t.Cleanup(func() {
				_ = fs.unixFS.Close()
				_ = os.RemoveAll(rfs.root)
			})
			if err := rfs.CreateServerFile("test.txt", tc.existing); err != nil {
				t.Fatal(err)
			}
			if tc.otherSize > 0 {
				if err := rfs.CreateServerFile("other.txt", bytes.Repeat([]byte("x"), tc.otherSize)); err != nil {
					t.Fatal(err)
				}
			}
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			if _, err := writer.Write(tc.content); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := rfs.CreateServerFile("test.txt.gz", compressed.Bytes()); err != nil {
				t.Fatal(err)
			}
			usage, err := fs.DiskUsage(false)
			if err != nil {
				t.Fatal(err)
			}
			limit := tc.limit
			if limit == 0 {
				limit = usage + 1
			}
			fs.SetDiskLimit(limit)
			if err := fs.DecompressFile(context.Background(), "/", "test.txt.gz"); !IsErrorCode(err, ErrCodeDiskSpace) {
				t.Fatalf("expected disk space error, got %v", err)
			}
			content, err := os.ReadFile(filepath.Join(rfs.root, "server", "test.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(content, tc.existing) {
				t.Fatal("quota failure changed the existing contents")
			}
			if got := fs.CachedUsage(); got != usage {
				t.Fatalf("quota failure changed cached usage: got %d, want %d", got, usage)
			}
		})
	}
}

// Given an archive named test.{ext}, with the following file structure:
//
//	test/
//	|──inside/
//	|────finside.txt
//	|──outside.txt
//
// this test will ensure that it's being decompressed as expected
func TestFilesystem_DecompressFile(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("Decompress", func() {
		for _, ext := range []string{"zip", "rar", "tar", "tar.gz"} {
			g.It("can decompress a "+ext, func() {
				// copy the file to the new FS
				c, err := os.ReadFile("./testdata/test." + ext)
				g.Assert(err).IsNil()
				err = rfs.CreateServerFile("./test."+ext, c)
				g.Assert(err).IsNil()

				// decompress
				err = fs.DecompressFile(context.Background(), "/", "test."+ext)
				g.Assert(err).IsNil()

				// make sure everything is where it is supposed to be
				_, err = rfs.StatServerFile("test/outside.txt")
				g.Assert(err).IsNil()

				st, err := rfs.StatServerFile("test/inside")
				g.Assert(err).IsNil()
				g.Assert(st.IsDir()).IsTrue()

				_, err = rfs.StatServerFile("test/inside/finside.txt")
				g.Assert(err).IsNil()
				g.Assert(st.IsDir()).IsTrue()
			})
		}

		g.AfterEach(func() {
			_ = fs.TruncateRootDirectory()
		})
	})
}

// Empty directories have no file to create them implicitly, so extraction must
// create them explicitly or they are dropped.
func TestFilesystem_DecompressFileEmptyDirectory(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("Decompress", func() {
		archives := []struct {
			name  string
			build func() ([]byte, error)
		}{
			{"empty.zip", zipWithEmptyDir},
			{"empty.tar.gz", tarGzWithEmptyDir},
		}

		for _, a := range archives {
			g.It("preserves an empty directory in a "+a.name, func() {
				content, err := a.build()
				g.Assert(err).IsNil()
				err = rfs.CreateServerFile("./"+a.name, content)
				g.Assert(err).IsNil()

				err = fs.DecompressFile(context.Background(), "/", a.name)
				g.Assert(err).IsNil()

				// The empty directory must exist, and the sibling file must still extract.
				st, err := rfs.StatServerFile("empty")
				g.Assert(err).IsNil()
				g.Assert(st.IsDir()).IsTrue()

				_, err = rfs.StatServerFile("outside.txt")
				g.Assert(err).IsNil()
			})
		}

		g.AfterEach(func() {
			_ = fs.TruncateRootDirectory()
		})
	})
}

// zipWithEmptyDir builds a zip holding one file and an empty directory ("empty/").
func zipWithEmptyDir() ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	dh := &zip.FileHeader{Name: "empty/"}
	dh.SetMode(os.ModeDir | 0o755)
	if _, err := zw.CreateHeader(dh); err != nil {
		return nil, err
	}

	w, err := zw.Create("outside.txt")
	if err != nil {
		return nil, err
	}
	if _, err := w.Write([]byte("hello")); err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// tarGzWithEmptyDir builds a tar.gz holding one file and an empty directory ("empty/").
func tarGzWithEmptyDir() ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	if err := tw.WriteHeader(&tar.Header{Name: "empty/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		return nil, err
	}

	content := []byte("hello")
	if err := tw.WriteHeader(&tar.Header{Name: "outside.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(content); err != nil {
		return nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func TestFilesystem_DecompressionReplacementQuota(t *testing.T) {
	for _, format := range []string{"zip", "tar.gz"} {
		for _, tc := range []struct {
			name             string
			oldSize, newSize int
			reject           bool
		}{
			{"shrinking replacement", 1000, 800, false},
			{"same size replacement", 1000, 1000, false},
			{"growth fits exactly", 1000, 1100, false},
			{"growth exceeds quota", 1000, 1101, true},
			{"new file exceeds quota", 0, 800, true},
			{"empty replacement", 1000, 0, false},
		} {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				fs, rfs := NewFs()
				t.Cleanup(func() { _ = fs.unixFS.Close(); _ = os.RemoveAll(rfs.root) })
				if err := os.MkdirAll(filepath.Join(rfs.root, "server/nested"), 0o755); err != nil {
					t.Fatal(err)
				}
				old := bytes.Repeat([]byte("o"), tc.oldSize)
				content := bytes.Repeat([]byte("n"), tc.newSize)
				if tc.oldSize > 0 {
					if err := rfs.CreateServerFile("nested/test.txt", old); err != nil {
						t.Fatal(err)
					}
				}
				var archive bytes.Buffer
				if format == "zip" {
					zw := zip.NewWriter(&archive)
					w, err := zw.Create("nested/test.txt")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := w.Write(content); err != nil {
						t.Fatal(err)
					}
					if err := zw.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					gw := gzip.NewWriter(&archive)
					tw := tar.NewWriter(gw)
					if err := tw.WriteHeader(&tar.Header{Name: "nested/test.txt", Mode: 0o644, Size: int64(len(content))}); err != nil {
						t.Fatal(err)
					}
					if _, err := tw.Write(content); err != nil {
						t.Fatal(err)
					}
					if err := tw.Close(); err != nil {
						t.Fatal(err)
					}
					if err := gw.Close(); err != nil {
						t.Fatal(err)
					}
				}
				filename := "test." + format
				if err := rfs.CreateServerFile(filename, archive.Bytes()); err != nil {
					t.Fatal(err)
				}
				usage, err := fs.DiskUsage(false)
				if err != nil {
					t.Fatal(err)
				}
				fs.SetDiskLimit(usage + 100)
				err = fs.SpaceAvailableForDecompression(context.Background(), "/", filename)
				if tc.reject {
					if !IsErrorCode(err, ErrCodeDiskSpace) {
						t.Fatalf("expected quota error, got %v", err)
					}
					if fs.CachedUsage() != usage {
						t.Fatal("preflight changed usage")
					}
					if tc.oldSize > 0 {
						got, err := os.ReadFile(filepath.Join(rfs.root, "server/nested/test.txt"))
						if err != nil || !bytes.Equal(got, old) {
							t.Fatal("preflight changed existing contents")
						}
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := fs.DecompressFile(context.Background(), "/", filename); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(rfs.root, "server/nested/test.txt"))
				if err != nil || !bytes.Equal(got, content) {
					t.Fatalf("incorrect replacement contents: %v", err)
				}
				if want := usage + int64(tc.newSize-tc.oldSize); fs.CachedUsage() != want {
					t.Fatalf("usage %d, want %d", fs.CachedUsage(), want)
				}
			})
		}
	}
}
