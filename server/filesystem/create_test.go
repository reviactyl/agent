package filesystem

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreateFailureLeavesNoFileAndAllowsRetry(t *testing.T) {
	for _, failure := range []string{"read error", "short body", "quota"} {
		t.Run(failure, func(t *testing.T) {
			fs, rfs := NewFs()
			t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
			var reader io.Reader = strings.NewReader("partial")
			if failure == "read error" {
				reader = io.MultiReader(reader, streamReadFunc(func([]byte) (int, error) {
					return 0, errors.New("interrupted body")
				}))
			}
			if failure == "quota" {
				fs.SetDiskLimit(4)
			}
			require.Error(t, fs.Create("file.txt", reader, 20, 0o644))
			_, err := fs.UnixFS().Lstat("file.txt")
			require.True(t, os.IsNotExist(err))
			entries, err := fs.ReadDir("/")
			require.NoError(t, err)
			require.Empty(t, entries)
			require.Zero(t, fs.CachedUsage())
			fs.SetDiskLimit(8)
			require.NoError(t, fs.Create("file.txt", strings.NewReader("complete"), 8, 0o644))
			data, err := os.ReadFile(filepath.Join(rfs.root, "server", "file.txt"))
			require.NoError(t, err)
			require.Equal(t, "complete", string(data))
			require.Equal(t, int64(8), fs.CachedUsage())
		})
	}
}

func TestCreateFailurePreservesConcurrentDestination(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
	readErr := errors.New("interrupted body")
	reader := io.MultiReader(strings.NewReader("partial"), streamReadFunc(func([]byte) (int, error) {
		require.NoError(t, fs.Write("file.txt", strings.NewReader("other writer"), 12, 0o644))
		return 0, readErr
	}))

	require.ErrorIs(t, fs.Create("file.txt", reader, 20, 0o644), readErr)
	data, err := os.ReadFile(filepath.Join(rfs.root, "server", "file.txt"))
	require.NoError(t, err)
	require.Equal(t, "other writer", string(data))
	entries, err := fs.ReadDir("/")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, int64(12), fs.CachedUsage())
}

func TestCreateCommitPreservesConcurrentDestination(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
	reader := streamReadFunc(func(p []byte) (int, error) {
		require.NoError(t, fs.Write("file.txt", strings.NewReader("other writer"), 12, 0o644))
		return copy(p, "draft"), io.EOF
	})

	require.ErrorIs(t, fs.Create("file.txt", reader, 5, 0o644), os.ErrExist)
	data, err := os.ReadFile(filepath.Join(rfs.root, "server", "file.txt"))
	require.NoError(t, err)
	require.Equal(t, "other writer", string(data))
	entries, err := fs.ReadDir("/")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, int64(12), fs.CachedUsage())
}

func TestCreateEmptyFile(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })
	require.NoError(t, fs.Create("empty.txt", strings.NewReader(""), 0, 0o644))
	info, err := fs.UnixFS().Lstat("empty.txt")
	require.NoError(t, err)
	require.Zero(t, info.Size())
	require.Zero(t, fs.CachedUsage())
}
