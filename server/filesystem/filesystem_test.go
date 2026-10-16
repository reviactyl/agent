package filesystem

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"unicode/utf8"

	. "github.com/franela/goblin"

	"github.com/reviactyl/agent/internal/ufs"

	"github.com/reviactyl/agent/config"
)

func TestStatMarshalJSONModeBits(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
	for _, directory := range []bool{false, true} {
		for special := 0; special < 8; special++ {
			for _, permissions := range []os.FileMode{0o755, 0o644, 0o040, 0} {
				bits := os.FileMode(special<<9) | permissions
				t.Run(fmt.Sprintf("directory=%t/mode=%o", directory, bits), func(t *testing.T) {
					name := fmt.Sprintf("mode-%t-%o", directory, bits)
					p := filepath.Join(rfs.root, "server", name)
					if directory {
						if err := os.Mkdir(p, 0o755); err != nil {
							t.Fatal(err)
						}
					} else if err := os.WriteFile(p, []byte("test"), 0o644); err != nil {
						t.Fatal(err)
					}
					mode := permissions
					if special&4 != 0 {
						mode |= os.ModeSetuid
					}
					if special&2 != 0 {
						mode |= os.ModeSetgid
					}
					if special&1 != 0 {
						mode |= os.ModeSticky
					}
					if err := os.Chmod(p, mode); err != nil {
						t.Fatal(err)
					}
					info, err := fs.unixFS.Lstat(name)
					if err != nil {
						t.Fatal(err)
					}
					stat := Stat{FileInfo: info}
					data, err := json.Marshal(&stat)
					if err != nil {
						t.Fatal(err)
					}
					var result struct {
						ModeBits  string `json:"mode_bits"`
						Mode      string `json:"mode"`
						Directory bool   `json:"directory"`
					}
					if err := json.Unmarshal(data, &result); err != nil {
						t.Fatal(err)
					}
					if want := fmt.Sprintf("%o", bits); result.ModeBits != want {
						t.Fatalf("mode_bits = %q, want %q", result.ModeBits, want)
					}
					if result.Mode != info.Mode().String() || result.Directory != directory {
						t.Fatalf("file type or symbolic mode changed: %+v", result)
					}
				})
			}
		}
	}
}

func NewFs() (*Filesystem, *rootFs) {
	config.Set(&config.Configuration{
		AuthenticationToken: "abc",
		System: config.SystemConfiguration{
			RootDirectory:     "/server",
			DiskCheckInterval: 150,
		},
	})

	tmpDir, err := os.MkdirTemp(os.TempDir(), "reviactyl")
	if err != nil {
		panic(err)
		return nil, nil
	}

	rfs := rootFs{root: tmpDir}

	p := filepath.Join(tmpDir, "server")
	if err := os.Mkdir(p, 0o755); err != nil {
		panic(err)
	}

	fs, _ := New(p, 0, []string{})
	fs.isTest = true
	if err := fs.TruncateRootDirectory(); err != nil {
		panic(err)
	}

	return fs, &rfs
}

type rootFs struct {
	root string
}

func getFileContent(file ufs.File) string {
	var w bytes.Buffer
	if _, err := bufio.NewReader(file).WriteTo(&w); err != nil {
		panic(err)
	}
	return w.String()
}

func (rfs *rootFs) CreateServerFile(p string, c []byte) error {
	f, err := os.Create(filepath.Join(rfs.root, "server", p))

	if err == nil {
		_, _ = f.Write(c)
		_ = f.Close()
	}

	return err
}

func (rfs *rootFs) CreateServerFileFromString(p string, c string) error {
	return rfs.CreateServerFile(p, []byte(c))
}

func (rfs *rootFs) StatServerFile(p string) (os.FileInfo, error) {
	return os.Stat(filepath.Join(rfs.root, "server", p))
}

func TestFilesystem_Openfile(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("File", func() {
		g.It("returns custom error when file does not exist", func() {
			_, _, err := fs.File("foo/bar.txt")

			g.Assert(err).IsNotNil()
			// TODO
			//g.Assert(IsErrorCode(err, ErrNotExist)).IsTrue()
		})

		g.It("returns file stat information", func() {
			_ = rfs.CreateServerFile("foo.txt", []byte("hello world"))

			f, st, err := fs.File("foo.txt")
			g.Assert(err).IsNil()

			g.Assert(st.Name()).Equal("foo.txt")
			g.Assert(f).IsNotNil()
			_ = f.Close()
		})

		g.AfterEach(func() {
			_ = fs.TruncateRootDirectory()
		})
	})
}

func TestFilesystem_Touch(t *testing.T) {
	g := Goblin(t)
	fs, _ := NewFs()

	g.Describe("Touch", func() {
		g.It("enforces disk limits while writing to the returned handle", func() {
			fs.SetDiskLimit(10)

			f, err := fs.Touch("quota.txt", ufs.O_RDWR|ufs.O_TRUNC)
			g.Assert(err).IsNil()
			defer f.Close()

			n, err := f.WriteAt([]byte("1234567890"), 0)
			g.Assert(err).IsNil()
			g.Assert(n).Equal(10)
			g.Assert(fs.CachedUsage()).Equal(int64(10))

			n, err = f.WriteAt([]byte("1"), 10)
			g.Assert(err).IsNotNil()
			g.Assert(n).Equal(0)
			g.Assert(IsErrorCode(err, ErrCodeDiskSpace)).IsTrue()
			g.Assert(fs.CachedUsage()).Equal(int64(10))
		})

		g.It("enforces disk limits while sequentially writing to the returned handle", func() {
			fs.SetDiskLimit(10)

			f, err := fs.Touch("quota.txt", ufs.O_RDWR|ufs.O_TRUNC)
			g.Assert(err).IsNil()
			defer f.Close()

			n, err := f.Write([]byte("1234567890"))
			g.Assert(err).IsNil()
			g.Assert(n).Equal(10)

			n, err = f.Write([]byte("1"))
			g.Assert(err).IsNotNil()
			g.Assert(n).Equal(0)
			g.Assert(IsErrorCode(err, ErrCodeDiskSpace)).IsTrue()
			g.Assert(fs.CachedUsage()).Equal(int64(10))
		})

		g.It("updates disk usage when a truncated file is closed smaller", func() {
			r := bytes.NewReader([]byte("1234567890"))
			err := fs.Write("quota.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()
			g.Assert(fs.CachedUsage()).Equal(int64(10))

			f, err := fs.Touch("quota.txt", ufs.O_RDWR|ufs.O_TRUNC)
			g.Assert(err).IsNil()

			n, err := f.WriteAt([]byte("1234"), 0)
			g.Assert(err).IsNil()
			g.Assert(n).Equal(4)

			err = f.Close()
			g.Assert(err).IsNil()
			g.Assert(fs.CachedUsage()).Equal(int64(4))
		})

		g.It("does not reset disk usage after a failed huge-offset write", func() {
			const usage = int64(5 * 1024 * 1024)
			fs.SetDiskLimit(10 * 1024 * 1024)
			fs.unixFS.SetUsage(usage)

			f, err := fs.Touch("quota.txt", ufs.O_RDWR|ufs.O_TRUNC)
			g.Assert(err).IsNil()

			n, err := f.WriteAt([]byte("x"), math.MaxInt64)
			g.Assert(err).IsNotNil()
			g.Assert(n).Equal(0)

			err = f.Close()
			g.Assert(err).IsNil()
			g.Assert(fs.CachedUsage()).Equal(usage)
		})

		g.AfterEach(func() {
			_ = fs.TruncateRootDirectory()
		})
	})
}

func TestFilesystem_Writefile(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("Open and WriteFile", func() {
		buf := &bytes.Buffer{}

		// Test that a file can be written to the disk and that the disk space used as a result
		// is updated correctly in the end.
		g.It("can create a new file", func() {
			r := bytes.NewReader([]byte("test file content"))

			g.Assert(fs.CachedUsage()).Equal(int64(0))

			err := fs.Write("test.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			f, _, err := fs.File("test.txt")
			g.Assert(err).IsNil()
			defer f.Close()
			g.Assert(getFileContent(f)).Equal("test file content")
			g.Assert(fs.CachedUsage()).Equal(r.Size())
		})

		g.It("can create a new file inside a nested directory with leading slash", func() {
			r := bytes.NewReader([]byte("test file content"))

			err := fs.Write("/some/nested/test.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			f, _, err := fs.File("/some/nested/test.txt")
			g.Assert(err).IsNil()
			defer f.Close()
			g.Assert(getFileContent(f)).Equal("test file content")
		})

		g.It("can create a new file inside a nested directory without a trailing slash", func() {
			r := bytes.NewReader([]byte("test file content"))

			err := fs.Write("some/../foo/bar/test.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			f, _, err := fs.File("foo/bar/test.txt")
			g.Assert(err).IsNil()
			defer f.Close()
			g.Assert(getFileContent(f)).Equal("test file content")
		})

		g.It("cannot create a file outside the root directory", func() {
			r := bytes.NewReader([]byte("test file content"))

			err := fs.Write("/some/../foo/../../test.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrBadPathResolution)).IsTrue("err is not ErrBadPathResolution")
		})

		g.It("cannot write a file that exceeds the disk limits", func() {
			fs.SetDiskLimit(1024)

			b := make([]byte, 1025)
			_, err := rand.Read(b)
			g.Assert(err).IsNil()
			g.Assert(len(b)).Equal(1025)

			r := bytes.NewReader(b)
			err = fs.Write("test.txt", r, int64(len(b)), 0o644)
			g.Assert(err).IsNotNil()
			g.Assert(IsErrorCode(err, ErrCodeDiskSpace)).IsTrue()
		})

		g.It("cannot write a file whose claimed size overflows the quota check", func() {
			fs.SetDiskLimit(1024)
			fs.unixFS.SetUsage(1)

			r := bytes.NewReader([]byte("small body"))
			err := fs.Write("overflow.txt", r, math.MaxInt64, 0o644)
			g.Assert(err).IsNotNil()
			g.Assert(IsErrorCode(err, ErrCodeDiskSpace)).IsTrue()

			_, err = rfs.StatServerFile("overflow.txt")
			g.Assert(errors.Is(err, os.ErrNotExist)).IsTrue("err is not os.ErrNotExist")
		})

		g.It("truncates the file when writing new contents", func() {
			r := bytes.NewReader([]byte("original data"))
			err := fs.Write("test.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			r = bytes.NewReader([]byte("new data"))
			err = fs.Write("test.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			f, _, err := fs.File("test.txt")
			g.Assert(err).IsNil()
			defer f.Close()
			g.Assert(getFileContent(f)).Equal("new data")
		})

		g.AfterEach(func() {
			buf.Truncate(0)
			_ = fs.TruncateRootDirectory()
		})
	})
}

func TestFilesystem_CreateDirectory(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("CreateDirectory", func() {
		g.It("should create missing directories automatically", func() {
			err := fs.CreateDirectory("test", "foo/bar/baz")
			g.Assert(err).IsNil()

			st, err := rfs.StatServerFile("foo/bar/baz/test")
			g.Assert(err).IsNil()
			g.Assert(st.IsDir()).IsTrue()
			g.Assert(st.Name()).Equal("test")
		})

		g.It("should work with leading and trailing slashes", func() {
			err := fs.CreateDirectory("test", "/foozie/barzie/bazzy/")
			g.Assert(err).IsNil()

			st, err := rfs.StatServerFile("foozie/barzie/bazzy/test")
			g.Assert(err).IsNil()
			g.Assert(st.IsDir()).IsTrue()
			g.Assert(st.Name()).Equal("test")
		})

		g.It("should not allow the creation of directories outside the root", func() {
			err := fs.CreateDirectory("test", "e/../../something")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrBadPathResolution)).IsTrue("err is not ErrBadPathResolution")
		})

		g.It("should not increment the disk usage", func() {
			err := fs.CreateDirectory("test", "/")
			g.Assert(err).IsNil()
			g.Assert(fs.CachedUsage()).Equal(int64(0))
		})

		g.AfterEach(func() {
			_ = fs.TruncateRootDirectory()
		})
	})
}

func TestFilesystem_RenameParentOwnership(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
	fs.isTest = false
	cfg := config.Get()
	cfg.System.User.Uid = 65534
	cfg.System.User.Gid = 65534
	config.Set(cfg)
	if err := rfs.CreateServerFileFromString("config.txt", "config"); err != nil {
		t.Fatal(err)
	}
	chownErr := os.Chown(filepath.Join(rfs.root, "server", "config.txt"), 65534, 65534)
	if chownErr != nil && !errors.Is(chownErr, os.ErrPermission) {
		t.Fatal(chownErr)
	}
	if err := os.Mkdir(filepath.Join(rfs.root, "mode-reference"), 0o755); err != nil {
		t.Fatal(err)
	}
	reference, err := os.Stat(filepath.Join(rfs.root, "mode-reference"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rfs.root, "server", "existing"), 0o700); err != nil {
		t.Fatal(err)
	}
	err = fs.Rename("config.txt", "existing/fresh/nested/config.txt")
	if chownErr != nil {
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("rename should stop on failed ownership correction, got %v", err)
		}
		if _, err := rfs.StatServerFile("config.txt"); err != nil {
			t.Fatalf("source was moved after failed ownership correction: %v", err)
		}
		if _, err := rfs.StatServerFile("existing/fresh"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("new parents remain after failed ownership correction: %v", err)
		}
		if err := fs.Rename("config.txt", "existing/fresh/nested/config.txt"); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("retry bypassed ownership correction: %v", err)
		}
		if _, err := rfs.StatServerFile("config.txt"); err != nil {
			t.Fatalf("retry moved the source after failed ownership correction: %v", err)
		}
		if _, err := rfs.StatServerFile("existing/fresh"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("new parents remain after retry: %v", err)
		}
		if info, err := rfs.StatServerFile("existing"); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("existing parent changed after failed rename: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"existing", "existing/fresh", "existing/fresh/nested"} {
		info, err := rfs.StatServerFile(path)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		uid, gid, mode := uint32(65534), uint32(65534), reference.Mode().Perm()
		if path == "existing" {
			uid, gid, mode = uint32(os.Geteuid()), uint32(os.Getegid()), 0o700
		}
		if stat.Uid != uid || stat.Gid != gid || info.Mode().Perm() != mode {
			t.Fatalf("%s: got ownership %d:%d mode %o, want %d:%d mode %o", path, stat.Uid, stat.Gid, info.Mode().Perm(), uid, gid, mode)
		}
	}
	if _, err := rfs.StatServerFile("existing/fresh/nested/config.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystem_Rename(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("Rename", func() {
		g.BeforeEach(func() {
			if err := rfs.CreateServerFileFromString("source.txt", "text content"); err != nil {
				panic(err)
			}
		})

		g.It("returns an error if the target already exists", func() {
			err := rfs.CreateServerFileFromString("target.txt", "taget content")
			g.Assert(err).IsNil()

			err = fs.Rename("source.txt", "target.txt")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrExist)).IsTrue("err is not ErrExist")
		})

		g.It("returns an error if the final destination is the root directory", func() {
			err := fs.Rename("source.txt", "/")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrBadPathResolution)).IsTrue("err is not ErrBadPathResolution")
		})

		g.It("returns an error if the source destination is the root directory", func() {
			err := fs.Rename("/", "target.txt")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrBadPathResolution)).IsTrue("err is not ErrBadPathResolution")
		})

		g.It("does not allow renaming to a location outside the root", func() {
			err := fs.Rename("source.txt", "../target.txt")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrBadPathResolution)).IsTrue("err is not ErrBadPathResolution")
		})

		g.It("does not allow renaming from a location outside the root", func() {
			err := rfs.CreateServerFileFromString("/../ext-source.txt", "taget content")
			g.Assert(err).IsNil()

			err = fs.Rename("/../ext-source.txt", "target.txt")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrBadPathResolution)).IsTrue("err is not ErrBadPathResolution")
		})

		g.It("allows a file to be renamed", func() {
			err := fs.Rename("source.txt", "target.txt")
			g.Assert(err).IsNil()

			_, err = rfs.StatServerFile("source.txt")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrNotExist)).IsTrue("err is not ErrNotExist")

			st, err := rfs.StatServerFile("target.txt")
			g.Assert(err).IsNil()
			g.Assert(st.Name()).Equal("target.txt")
			g.Assert(st.Size()).IsNotZero()
		})

		g.It("allows a folder to be renamed", func() {
			err := os.Mkdir(filepath.Join(rfs.root, "/server/source_dir"), 0o755)
			g.Assert(err).IsNil()

			err = fs.Rename("source_dir", "target_dir")
			g.Assert(err).IsNil()

			_, err = rfs.StatServerFile("source_dir")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrNotExist)).IsTrue("err is not ErrNotExist")

			st, err := rfs.StatServerFile("target_dir")
			g.Assert(err).IsNil()
			g.Assert(st.IsDir()).IsTrue()
		})

		g.It("returns an error if the source does not exist", func() {
			err := fs.Rename("missing.txt", "target.txt")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrNotExist)).IsTrue("err is not ErrNotExist")
		})

		g.It("creates directories if they are missing", func() {
			err := fs.Rename("source.txt", "nested/folder/target.txt")
			g.Assert(err).IsNil()

			st, err := rfs.StatServerFile("nested/folder/target.txt")
			g.Assert(err).IsNil()
			g.Assert(st.Name()).Equal("target.txt")
		})

		g.AfterEach(func() {
			_ = fs.TruncateRootDirectory()
		})
	})
}

func TestFilesystem_Copy(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("Copy", func() {
		g.BeforeEach(func() {
			if err := rfs.CreateServerFileFromString("source.txt", "text content"); err != nil {
				panic(err)
			}

			fs.unixFS.SetUsage(int64(utf8.RuneCountInString("test content")))
		})

		g.It("should return an error if the source does not exist", func() {
			err := fs.Copy("foo.txt")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrNotExist)).IsTrue("err is not ErrNotExist")
		})

		g.It("should return an error if the source is outside the root", func() {
			err := rfs.CreateServerFileFromString("/../ext-source.txt", "text content")
			g.Assert(err).IsNil()

			err = fs.Copy("../ext-source.txt")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrBadPathResolution)).IsTrue("err is not ErrBadPathResolution")
		})

		g.It("should return an error if the source directory is outside the root", func() {
			err := os.MkdirAll(filepath.Join(rfs.root, "nested/in/dir"), 0o755)
			g.Assert(err).IsNil()

			err = rfs.CreateServerFileFromString("/../nested/in/dir/ext-source.txt", "external content")
			g.Assert(err).IsNil()

			err = fs.Copy("../nested/in/dir/ext-source.txt")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrBadPathResolution)).IsTrue("err is not ErrBadPathResolution")

			err = fs.Copy("nested/in/../../../nested/in/dir/ext-source.txt")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrBadPathResolution)).IsTrue("err is not ErrBadPathResolution")
		})

		g.It("should return an error if the source is a directory", func() {
			err := os.Mkdir(filepath.Join(rfs.root, "server/dir"), 0o755)
			g.Assert(err).IsNil()

			err = fs.Copy("dir")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrNotExist)).IsTrue("err is not ErrNotExist")
		})

		g.It("should return an error if there is not space to copy the file", func() {
			fs.SetDiskLimit(2)

			err := fs.Copy("source.txt")
			g.Assert(err).IsNotNil()
			g.Assert(IsErrorCode(err, ErrCodeDiskSpace)).IsTrue("err is not ErrCodeDiskSpace")
		})

		g.It("should create a copy of the file and increment the disk used", func() {
			err := fs.Copy("source.txt")
			g.Assert(err).IsNil()

			_, err = rfs.StatServerFile("source.txt")
			g.Assert(err).IsNil()

			_, err = rfs.StatServerFile("source copy.txt")
			g.Assert(err).IsNil()
		})

		g.It("should create a copy of the file with a suffix if a copy already exists", func() {
			err := fs.Copy("source.txt")
			g.Assert(err).IsNil()

			err = fs.Copy("source.txt")
			g.Assert(err).IsNil()

			r := []string{"source.txt", "source copy.txt", "source copy 1.txt"}

			for _, name := range r {
				_, err = rfs.StatServerFile(name)
				g.Assert(err).IsNil()
			}

			g.Assert(fs.CachedUsage()).Equal(int64(utf8.RuneCountInString("test content")) * 3)
		})

		g.It("should create a copy inside of a directory", func() {
			err := os.MkdirAll(filepath.Join(rfs.root, "/server/nested/in/dir"), 0o755)
			g.Assert(err).IsNil()

			err = rfs.CreateServerFileFromString("nested/in/dir/source.txt", "test content")
			g.Assert(err).IsNil()

			err = fs.Copy("nested/in/dir/source.txt")
			g.Assert(err).IsNil()

			_, err = rfs.StatServerFile("nested/in/dir/source.txt")
			g.Assert(err).IsNil()

			_, err = rfs.StatServerFile("nested/in/dir/source copy.txt")
			g.Assert(err).IsNil()
		})

		g.AfterEach(func() {
			_ = fs.TruncateRootDirectory()
		})
	})
}

func TestFilesystem_Delete(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("Delete", func() {
		g.BeforeEach(func() {
			if err := rfs.CreateServerFileFromString("source.txt", "test content"); err != nil {
				panic(err)
			}

			fs.unixFS.SetUsage(int64(utf8.RuneCountInString("test content")))
		})

		g.It("does not delete files outside the root directory", func() {
			err := rfs.CreateServerFileFromString("/../ext-source.txt", "external content")
			g.Assert(err).IsNil()

			err = fs.Delete("../ext-source.txt")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrBadPathResolution)).IsTrue("err is not ErrBadPathResolution")
		})

		g.It("does not allow the deletion of the root directory", func() {
			err := fs.Delete("/")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrBadPathResolution)).IsTrue("err is not ErrBadPathResolution")
		})

		g.It("does not return an error if the target does not exist", func() {
			err := fs.Delete("missing.txt")
			g.Assert(err).IsNil()

			st, err := rfs.StatServerFile("source.txt")
			g.Assert(err).IsNil()
			g.Assert(st.Name()).Equal("source.txt")
		})

		g.It("deletes files and subtracts their size from the disk usage", func() {
			err := fs.Delete("source.txt")
			g.Assert(err).IsNil()

			_, err = rfs.StatServerFile("source.txt")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrNotExist)).IsTrue("err is not ErrNotExist")

			g.Assert(fs.CachedUsage()).Equal(int64(0))
		})

		g.It("deletes all items inside a directory if the directory is deleted", func() {
			sources := []string{
				"foo/source.txt",
				"foo/bar/source.txt",
				"foo/bar/baz/source.txt",
			}

			err := os.MkdirAll(filepath.Join(rfs.root, "/server/foo/bar/baz"), 0o755)
			g.Assert(err).IsNil()

			for _, s := range sources {
				err = rfs.CreateServerFileFromString(s, "test content")
				g.Assert(err).IsNil()
			}

			fs.unixFS.SetUsage(int64(utf8.RuneCountInString("test content") * 3))

			err = fs.Delete("foo")
			g.Assert(err).IsNil()
			g.Assert(fs.unixFS.Usage()).Equal(int64(0))

			for _, s := range sources {
				_, err = rfs.StatServerFile(s)
				g.Assert(err).IsNotNil()
				g.Assert(errors.Is(err, ufs.ErrNotExist)).IsTrue("err is not ErrNotExist")
			}
		})

		g.It("deletes a symlink but not it's target within the root directory", func() {
			// Symlink to a file inside the root directory.
			err := os.Symlink(filepath.Join(rfs.root, "server/source.txt"), filepath.Join(rfs.root, "server/symlink.txt"))
			g.Assert(err).IsNil()

			// Delete the symlink itself.
			err = fs.Delete("symlink.txt")
			g.Assert(err).IsNil()

			// Ensure the symlink was deleted.
			_, err = os.Lstat(filepath.Join(rfs.root, "server/symlink.txt"))
			g.Assert(err).IsNotNil()

			// Ensure the symlink target still exists.
			_, err = os.Lstat(filepath.Join(rfs.root, "server/source.txt"))
			g.Assert(err).IsNil()
		})

		g.It("does not delete files symlinked outside of the root directory", func() {
			// Create a file outside the root directory.
			err := rfs.CreateServerFileFromString("/../source.txt", "test content")
			g.Assert(err).IsNil()

			// Create a symlink to the file outside the root directory.
			err = os.Symlink(filepath.Join(rfs.root, "source.txt"), filepath.Join(rfs.root, "/server/symlink.txt"))
			g.Assert(err).IsNil()

			// Delete the symlink. (This should pass as we will delete the symlink itself, not it's target)
			err = fs.Delete("symlink.txt")
			g.Assert(err).IsNil()

			// Ensure the file outside the root directory still exists.
			_, err = os.Lstat(filepath.Join(rfs.root, "source.txt"))
			g.Assert(err).IsNil()
		})

		g.It("does not delete files symlinked through a directory outside of the root directory", func() {
			// Create a directory outside the root directory.
			err := os.Mkdir(filepath.Join(rfs.root, "foo"), 0o755)
			g.Assert(err).IsNil()

			// Create a file inside the directory that is outside the root.
			err = rfs.CreateServerFileFromString("/../foo/source.txt", "test content")
			g.Assert(err).IsNil()

			// Symlink the directory that is outside the root to a file inside the root.
			err = os.Symlink(filepath.Join(rfs.root, "foo"), filepath.Join(rfs.root, "server/symlink"))
			g.Assert(err).IsNil()

			// Delete a file inside the symlinked directory.
			err = fs.Delete("symlink/source.txt")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrBadPathResolution)).IsTrue("err is not ErrBadPathResolution")

			// Ensure the file outside the root directory still exists.
			_, err = os.Lstat(filepath.Join(rfs.root, "foo/source.txt"))
			g.Assert(err).IsNil()
		})

		g.It("returns an error when trying to delete a non-existent file symlinked through a directory outside of the root directory", func() {
			// Create a directory outside the root directory.
			err := os.Mkdir(filepath.Join(rfs.root, "foo2"), 0o755)
			g.Assert(err).IsNil()

			// Symlink the directory that is outside the root to a file inside the root.
			err = os.Symlink(filepath.Join(rfs.root, "foo2"), filepath.Join(rfs.root, "server/symlink"))
			g.Assert(err).IsNil()

			// Delete a file inside the symlinked directory.
			err = fs.Delete("symlink/source.txt")
			g.Assert(err).IsNotNil()
			g.Assert(errors.Is(err, ufs.ErrBadPathResolution)).IsTrue("err is not ErrBadPathResolution")
		})

		g.AfterEach(func() {
			_ = fs.TruncateRootDirectory()
		})
	})
}

func TestWriteUnknownSize(t *testing.T) {
	for _, tt := range []struct {
		name       string
		contents   string
		limit      int64
		quotaError bool
	}{
		{name: "stream to EOF", contents: "1234567890"},
		{name: "empty stream"},
		{name: "replace with staging space", contents: "12345", limit: 17},
		{name: "reject beyond quota", contents: "1234567890123", limit: 12, quotaError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fs, rfs := NewFs()
			t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
			if err := fs.Write("nested/file.txt", bytes.NewBufferString("old contents"), 12, 0o644); err != nil {
				t.Fatal(err)
			}
			fs.SetDiskLimit(tt.limit)
			err := fs.Write("nested/file.txt", bytes.NewBufferString(tt.contents), -1, 0o644)
			if tt.quotaError {
				if !IsErrorCode(err, ErrCodeDiskSpace) {
					t.Fatalf("expected quota error, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(rfs.root, "server", "nested", "file.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if tt.quotaError && string(data) != "old contents" {
				t.Fatalf("failed replacement changed original: %q", data)
			}
			if !tt.quotaError && string(data) != tt.contents {
				t.Fatalf("contents = %q, want %q", data, tt.contents)
			}
			if usage := fs.CachedUsage(); usage != int64(len(data)) {
				t.Fatalf("cached usage = %d, actual = %d", usage, len(data))
			}
			if tt.limit > 0 && int64(len(data)) > tt.limit {
				t.Fatal("disk quota exceeded")
			}
		})
	}
}

type streamReadFunc func([]byte) (int, error)

func (r streamReadFunc) Read(p []byte) (int, error) { return r(p) }

func TestWriteStreamPreservesFileOnReadError(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
	if err := fs.Write("file.txt", bytes.NewBufferString("original"), 8, 0o644); err != nil {
		t.Fatal(err)
	}
	readErr := errors.New("stream interrupted")
	reads := 0
	reader := streamReadFunc(func(p []byte) (int, error) {
		reads++
		if reads == 1 {
			return copy(p, "partial replacement"), nil
		}
		return 0, readErr
	})
	if err := fs.Write("file.txt", reader, -1, 0o644); !errors.Is(err, readErr) {
		t.Fatalf("error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(rfs.root, "server", "file.txt"))
	if err != nil || string(data) != "original" {
		t.Fatalf("original changed: %q, %v", data, err)
	}
	entries, err := fs.ReadDir("/")
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging file was not cleaned up: %v, %v", entries, err)
	}
	if fs.CachedUsage() != 8 {
		t.Fatalf("usage = %d", fs.CachedUsage())
	}
}

func TestWriteStreamQuotaWithDiskScans(t *testing.T) {
	for _, limit := range []int64{100, 140, 150} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			fs, rfs := NewFs()
			t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
			for name, size := range map[string]int{"file.txt": 40, "other.txt": 60} {
				if err := fs.Write(name, bytes.NewReader(bytes.Repeat([]byte("x"), size)), int64(size), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			fs.SetDiskLimit(limit)
			reads := 0
			reader := streamReadFunc(func(p []byte) (int, error) {
				reads++
				size, err := fs.updateCachedDiskUsage()
				if err != nil {
					return 0, err
				}
				if size != fs.CachedUsage() || size > limit {
					t.Fatalf("scan size=%d, cached=%d, limit=%d", size, fs.CachedUsage(), limit)
				}
				switch reads {
				case 1:
					return copy(p, bytes.Repeat([]byte("y"), 20)), nil
				case 2:
					return copy(p, bytes.Repeat([]byte("y"), 30)), nil
				default:
					return 0, io.EOF
				}
			})
			err := fs.Write("file.txt", reader, -1, 0o644)
			if limit < 150 {
				if !IsErrorCode(err, ErrCodeDiskSpace) {
					t.Fatalf("expected quota error, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(rfs.root, "server", "file.txt"))
			if err != nil {
				t.Fatal(err)
			}
			want := bytes.Repeat([]byte("x"), 40)
			if limit == 150 {
				want = bytes.Repeat([]byte("y"), 50)
			}
			if !bytes.Equal(data, want) {
				t.Fatalf("unexpected destination: %q", data)
			}
			usage := fs.CachedUsage()
			scanned, err := fs.updateCachedDiskUsage()
			if err != nil || usage != scanned || scanned != int64(60+len(want)) {
				t.Fatalf("usage=%d, actual=%d, error=%v", usage, scanned, err)
			}
			entries, err := fs.ReadDir("/")
			if err != nil || len(entries) != 2 {
				t.Fatalf("staging file was not cleaned up: %v, %v", entries, err)
			}
		})
	}
}

func TestWritePreservesExistingPermissions(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o700, 0o640} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("mode=%o/streaming=%t", mode, streaming), func(t *testing.T) {
				fs, rfs := NewFs()
				t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
				if err := fs.Write("script.sh", bytes.NewBufferString("old"), 3, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := fs.Chmod("script.sh", mode); err != nil {
					t.Fatal(err)
				}
				contents := "#!/bin/sh\nprintf 'updated\\n'\n"
				size := int64(len(contents))
				if streaming {
					size = -1
				}
				if err := fs.Write("script.sh", bytes.NewBufferString(contents), size, 0o644); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(rfs.root, "server", "script.sh")
				st, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if st.Mode().Perm() != mode {
					t.Fatalf("permissions = %o, want %o", st.Mode().Perm(), mode)
				}
				if mode&0o100 != 0 {
					output, err := exec.Command(path).Output()
					if err != nil || string(output) != "updated\n" {
						t.Fatalf("script execution = %q, error = %v", output, err)
					}
				}
			})
		}
	}
}
