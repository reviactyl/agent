package filesystem

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mholt/archives"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reviactyl/agent/internal/ufs"
)

// A walk that fails part of the way through has only counted some of the files,
// so it must not lower the cached usage.
func TestFailedDiskUsageWalkDoesNotLowerUsage(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })

	fs.unixFS.SetUsage(12345)
	require.NoError(t, os.RemoveAll(filepath.Join(rfs.root, "server")))

	_, err := fs.DiskUsage(false)
	require.Error(t, err)
	assert.Equal(t, int64(12345), fs.CachedUsage())
}

// A caller that found the cache stale and waited for another walk to finish
// uses that walk's result instead of walking the disk again.
func TestQueuedDiskUsageLookupUsesRefreshedCache(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })

	require.NoError(t, rfs.CreateServerFile("a", randomData(t, 4096)))

	// Hold the lookup lock so that the caller sees a stale cache and waits.
	fs.lookupMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := fs.DiskUsage(false)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)

	// While the caller waits, another walk finishes and refreshes the cache.
	fs.lastLookupTime.Set(time.Now())
	fs.unixFS.SetUsage(12345)
	fs.lookupMu.Unlock()

	require.NoError(t, <-done)
	assert.Equal(t, int64(12345), fs.CachedUsage())
}

// Real os.File handles return *syscall.Stat_t from Stat, which must still be
// treated as an inode the walk can account for.
func TestFileInodeAcceptsOSFileStat(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })

	f, err := fs.Touch("upload.bin", ufs.O_RDWR)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	inner := f.(*quotaFile).File
	info, err := inner.Stat()
	require.NoError(t, err)
	_, isSyscall := info.Sys().(*syscall.Stat_t)
	require.True(t, isSyscall, "os.File.Stat should yield *syscall.Stat_t")

	ino, ok := fileInode(info)
	require.True(t, ok)
	require.NotZero(t, ino)
}

// An SFTP-style upload that finishes after its directory has already been
// listed must still be included in the walk total, otherwise later writes
// can exceed the disk limit.
func TestWalkCountsUploadCreatedAfterDirectoryVisit(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })

	require.NoError(t, rfs.CreateServerFile("a.bin", []byte("0123456789")))
	fs.SetDiskLimit(100)
	usage, err := fs.DiskUsage(false)
	require.NoError(t, err)
	require.Equal(t, int64(10), usage)

	var once sync.Once
	fs.walkEntryHook = func() {
		once.Do(func() {
			f, err := fs.Touch("upload.bin", ufs.O_RDWR)
			require.NoError(t, err)
			_, err = f.Write(bytes.Repeat([]byte("x"), 80))
			require.NoError(t, err)
			require.NoError(t, f.Close())
		})
	}

	usage, err = fs.updateCachedDiskUsage()
	require.NoError(t, err)
	require.Equal(t, int64(90), usage)
	require.Equal(t, int64(90), fs.CachedUsage())
	require.Equal(t, int64(90), diskUsage(t, rfs))

	err = fs.Write("extra.bin", bytes.NewReader(bytes.Repeat([]byte("y"), 20)), 20, 0o644)
	require.Error(t, err)
	assert.True(t, IsErrorCode(err, ErrCodeDiskSpace), "extra write should be rejected as over quota")
	assert.Equal(t, int64(90), diskUsage(t, rfs))
	assert.Equal(t, int64(90), fs.CachedUsage())
}

func TestWalkReconcilesStagedReplacementAndCleanup(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("readFailure=", fail), func(t *testing.T) {
			fs, rfs := NewFs()
			t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
			require.NoError(t, rfs.CreateServerFile("a.bin", bytes.Repeat([]byte("a"), 80)))
			fs.SetDiskLimit(100)
			_, err := fs.DiskUsage(false)
			require.NoError(t, err)
			var once sync.Once
			fs.walkEntryHook = func() {
				once.Do(func() {
					var reader io.Reader = bytes.NewReader(bytes.Repeat([]byte("b"), 80))
					if fail {
						reader = io.MultiReader(reader, failingScanReader{})
					}
					_, err := fs.writeStream("a.bin", reader, 0o644, true)
					if fail {
						require.Error(t, err)
					} else {
						require.NoError(t, err)
					}
				})
			}
			usage, err := fs.updateCachedDiskUsage()
			require.NoError(t, err)
			require.Equal(t, int64(80), usage)
			require.Equal(t, diskUsage(t, rfs), fs.CachedUsage())
			require.NoError(t, fs.Write("extra.bin", bytes.NewReader(bytes.Repeat([]byte("c"), 20)), 20, 0o644))
			require.Equal(t, int64(100), diskUsage(t, rfs))
		})
	}
}

type failingScanReader struct{}

func (failingScanReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type pausedTruncateFile struct {
	ufs.File
	truncated chan struct{}
	resume    chan struct{}
}

func (f pausedTruncateFile) Truncate(size int64) error {
	err := f.File.Truncate(size)
	close(f.truncated)
	<-f.resume
	return err
}

func TestReplaceContentsCommitsAccountingBeforeScan(t *testing.T) {
	for _, sizes := range [][2]int{{80, 40}, {40, 80}} {
		t.Run(fmt.Sprint(sizes), func(t *testing.T) {
			fs, rfs := NewFs()
			t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
			require.NoError(t, rfs.CreateServerFile("config", bytes.Repeat([]byte("a"), sizes[0])))
			fs.SetDiskLimit(100)
			_, err := fs.DiskUsage(false)
			require.NoError(t, err)
			file, err := fs.Touch("config", ufs.O_RDWR)
			require.NoError(t, err)
			defer file.Close()
			q := file.(*quotaFile)
			paused := pausedTruncateFile{File: q.File, truncated: make(chan struct{}), resume: make(chan struct{})}
			q.File = paused
			replaced := make(chan error, 1)
			go func() { replaced <- q.ReplaceContents(bytes.Repeat([]byte("b"), sizes[1])) }()
			<-paused.truncated
			scanned := make(chan error, 1)
			go func() { _, err := fs.updateCachedDiskUsage(); scanned <- err }()
			select {
			case err := <-scanned:
				close(paused.resume)
				require.NoError(t, <-replaced)
				require.NoError(t, err)
				t.Fatal("scan committed before replacement accounting")
			case <-time.After(50 * time.Millisecond):
			}
			close(paused.resume)
			require.NoError(t, <-replaced)
			require.NoError(t, <-scanned)
			require.Equal(t, int64(sizes[1]), fs.CachedUsage())
			require.Equal(t, diskUsage(t, rfs), fs.CachedUsage())
			if sizes[1] == 40 {
				require.Error(t, fs.Write("extra", bytes.NewReader(make([]byte, 70)), 70, 0o644))
			} else {
				require.NoError(t, fs.Write("extra", bytes.NewReader(make([]byte, 10)), 10, 0o644))
			}
		})
	}
}

func TestWalkCountsRenamedInodeOnce(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
	require.NoError(t, rfs.CreateServerFile("left.bin", nil))
	require.NoError(t, rfs.CreateServerFile("right.bin", nil))
	entries, err := fs.ReadDir("/")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	source, destination := entries[0].Name(), entries[1].Name()
	require.NoError(t, rfs.CreateServerFile(source, bytes.Repeat([]byte("a"), 80)))
	_, err = fs.DiskUsage(false)
	require.NoError(t, err)
	var once sync.Once
	fs.walkEntryHook = func() {
		once.Do(func() {
			fs.mu.Lock()
			defer fs.mu.Unlock()
			require.NoError(t, fs.unixFS.Replace(source, destination))
		})
	}
	usage, err := fs.updateCachedDiskUsage()
	require.NoError(t, err)
	require.Equal(t, int64(80), usage)
	require.Equal(t, diskUsage(t, rfs), usage)
}

func TestFailedStagedWriteRestoresCachedUsageWithoutWalk(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
	require.NoError(t, rfs.CreateServerFile("target", bytes.Repeat([]byte("a"), 80)))
	fs.SetDiskLimit(100)
	usage, err := fs.DiskUsage(false)
	require.NoError(t, err)
	require.False(t, fs.walking)
	reader := io.MultiReader(bytes.NewReader(bytes.Repeat([]byte("b"), 80)), failingScanReader{})
	_, err = fs.writeStream("target", reader, 0o644, true)
	require.Error(t, err)
	require.Equal(t, usage, fs.CachedUsage())
	require.Equal(t, diskUsage(t, rfs), fs.CachedUsage())
}

func TestHardlinkReplacementUpdatesActiveDiskScan(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = fs.unixFS.Close(); _ = os.RemoveAll(rfs.root) })
	require.NoError(t, rfs.CreateServerFile("source", []byte("source")))
	require.NoError(t, rfs.CreateServerFile("destination", bytes.Repeat([]byte("x"), 80)))
	previous, err := fs.unixFS.Lstat("destination")
	require.NoError(t, err)
	ino, ok := fileInode(previous)
	require.True(t, ok)
	_, err = fs.DiskUsage(false)
	require.NoError(t, err)
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	require.NoError(t, w.WriteHeader(&tar.Header{Name: "source", Mode: 0o644, Size: 6, Typeflag: tar.TypeReg}))
	_, err = w.Write([]byte("source"))
	require.NoError(t, err)
	require.NoError(t, w.WriteHeader(&tar.Header{Name: "destination", Linkname: "source", Typeflag: tar.TypeLink}))
	require.NoError(t, w.Close())
	replaced := false
	fs.walkEntryHook = func() {
		fs.mu.Lock()
		_, observed := fs.walkObserved[ino]
		fs.mu.Unlock()
		if replaced || !observed {
			return
		}
		replaced = true
		require.NoError(t, fs.extractStream(context.Background(), extractStreamOptions{Directory: "/", Format: archives.Tar{}, Reader: bytes.NewReader(archive.Bytes())}))
	}
	usage, err := fs.updateCachedDiskUsage()
	require.NoError(t, err)
	require.True(t, replaced)
	require.Equal(t, int64(6), usage)
	fs.walkEntryHook = nil
	fresh, err := fs.updateCachedDiskUsage()
	require.NoError(t, err)
	require.Equal(t, fresh, usage)
}
