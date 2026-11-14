package filesystem

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/mholt/archives"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reviactyl/agent/config"
)

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(data)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// A single compressed file decompressed over an existing, larger file leaves
// exactly the decompressed content behind.
func TestDecompressSingleFileReplacesExistingContent(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })

	require.NoError(t, rfs.CreateServerFileFromString("in.txt", strings.Repeat("A", 100)))
	require.NoError(t, rfs.CreateServerFile("in.txt.gz", gzipBytes(t, []byte("hello"))))

	require.NoError(t, fs.DecompressFile(context.Background(), "/", "in.txt.gz"))

	got, err := os.ReadFile(filepath.Join(rfs.root, "server", "in.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
}

// A single file decompressed from a compressed archive is owned by the server
// user, like every other extracted file.
func TestDecompressSingleFileOwnedByServerUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("giving files to another user needs root")
	}
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })

	const uid, gid = 1234, 1234
	cfg := config.Get()
	cfg.System.User.Uid = uid
	cfg.System.User.Gid = gid
	config.Set(cfg)
	fs.isTest = false

	require.NoError(t, rfs.CreateServerFile("in.txt.gz", gzipBytes(t, []byte("hello"))))
	require.NoError(t, fs.DecompressFile(context.Background(), "/", "in.txt.gz"))

	info, err := os.Stat(filepath.Join(rfs.root, "server", "in.txt"))
	require.NoError(t, err)
	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	assert.Equal(t, uint32(uid), st.Uid)
	assert.Equal(t, uint32(gid), st.Gid)
}

// A single compressed file that does not fit the disk limit is not left
// partly written.
func TestDecompressSingleFileStopsAtDiskLimit(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })

	const limit = 1 << 20
	fs.SetDiskLimit(limit)
	require.NoError(t, rfs.CreateServerFile("large.gz", gzipBytes(t, make([]byte, 8<<20))))

	err := fs.DecompressFile(context.Background(), "/", "large.gz")
	require.Error(t, err)
	assert.True(t, IsErrorCode(err, ErrCodeDiskSpace), "unexpected error: %v", err)

	_, err = os.Stat(filepath.Join(rfs.root, "server", "large"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.LessOrEqual(t, fs.CachedUsage(), int64(limit))
}

// Symlinks in an archive are extracted as symlinks, and special files are not
// extracted.
func TestDecompressExtractsEntriesByType(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "file.txt", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg}))
	_, err := tw.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "link", Linkname: "file.txt", Mode: 0o777, Typeflag: tar.TypeSymlink}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "hard", Linkname: "file.txt", Mode: 0o644, Typeflag: tar.TypeLink}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "device", Mode: 0o644, Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "pipe", Mode: 0o644, Typeflag: tar.TypeFifo}))
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, rfs.CreateServerFile("bundle.tar.gz", buf.Bytes()))

	require.NoError(t, fs.DecompressFile(context.Background(), "/", "bundle.tar.gz"))

	root := filepath.Join(rfs.root, "server")
	target, err := os.Readlink(filepath.Join(root, "link"))
	require.NoError(t, err)
	assert.Equal(t, "file.txt", target)
	content, err := os.ReadFile(filepath.Join(root, "hard"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(content))
	for _, name := range []string{"device", "pipe"} {
		_, err := os.Lstat(filepath.Join(root, name))
		assert.ErrorIs(t, err, os.ErrNotExist, name)
	}
}

// A symlink in a zip archive is extracted as a symlink.
func TestDecompressZipSymlinkEntry(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	hdr := &zip.FileHeader{Name: "link"}
	hdr.SetMode(iofs.ModeSymlink | 0o777)
	w, err := zw.CreateHeader(hdr)
	require.NoError(t, err)
	_, err = w.Write([]byte("target.txt"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, rfs.CreateServerFile("bundle.zip", buf.Bytes()))

	require.NoError(t, fs.DecompressFile(context.Background(), "/", "bundle.zip"))

	target, err := os.Readlink(filepath.Join(rfs.root, "server", "link"))
	require.NoError(t, err)
	assert.Equal(t, "target.txt", target)
}

type cancelingArchiveReader struct {
	io.Reader
	cancel context.CancelFunc
	read   int
}

func (r *cancelingArchiveReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	if r.read > 512 {
		r.cancel()
	}
	return n, err
}

func TestDecompressStopsWritingDuringCanceledEntry(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = fs.UnixFS().Close(); _ = os.RemoveAll(rfs.root) })

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	payload := bytes.Repeat([]byte("x"), 1<<20)
	for _, name := range []string{"large.txt", "later.txt"} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(payload))}))
		_, err := tw.Write(payload)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &cancelingArchiveReader{Reader: bytes.NewReader(archive.Bytes()), cancel: cancel}
	err := fs.extractStream(ctx, extractStreamOptions{
		Directory: "/",
		Format:    archives.Tar{},
		Reader:    source,
	})
	require.ErrorIs(t, err, context.Canceled)
	info, err := os.Stat(filepath.Join(rfs.root, "server", "large.txt"))
	require.NoError(t, err)
	assert.Zero(t, info.Size(), "canceled extraction wrote archive contents")
	_, err = os.Stat(filepath.Join(rfs.root, "server", "later.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
