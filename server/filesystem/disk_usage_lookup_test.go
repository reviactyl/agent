package filesystem

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

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
