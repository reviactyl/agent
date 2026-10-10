package filesystem

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	. "github.com/franela/goblin"
)

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
	fs, rfs := NewFs()
	t.Cleanup(func() {
		_ = fs.unixFS.Close()
		_ = os.RemoveAll(rfs.root)
	})
	oldContent := []byte("old contents")
	if err := rfs.CreateServerFile("test.txt", oldContent); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(bytes.Repeat([]byte("new"), 4096)); err != nil {
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
	fs.SetDiskLimit(usage + 1)
	if err := fs.DecompressFile(context.Background(), "/", "test.txt.gz"); !IsErrorCode(err, ErrCodeDiskSpace) {
		t.Fatalf("expected disk space error, got %v", err)
	}
	content, err := os.ReadFile(filepath.Join(rfs.root, "server", "test.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, oldContent) {
		t.Fatal("quota failure changed the existing contents")
	}
	if got := fs.CachedUsage(); got != usage {
		t.Fatalf("quota failure changed cached usage: got %d, want %d", got, usage)
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
