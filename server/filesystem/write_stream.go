package filesystem

import (
	"errors"
	"io"
	"path/filepath"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/reviactyl/agent/internal/ufs"
)

func (fs *Filesystem) writeStream(p string, r io.Reader, mode ufs.FileMode) (err error) {
	if err := fs.mkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	staged := filepath.Join(filepath.Dir(p), ".download-"+uuid.NewString())
	file, err := fs.unixFS.OpenFile(staged, ufs.O_WRONLY|ufs.O_CREATE|ufs.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		if cleanupErr := fs.unixFS.Remove(staged); !errors.Is(cleanupErr, ufs.ErrNotExist) {
			err = errors.Join(err, cleanupErr)
		}
	}()
	_, copyErr := io.Copy(streamWriter{fs: fs, file: file}, r)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := fs.chownFile(staged); err != nil {
		return err
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	var removedSize int64
	st, err := fs.unixFS.Lstat(p)
	if err != nil && !errors.Is(err, ufs.ErrNotExist) {
		return err
	}
	if err == nil && st.Mode().IsRegular() {
		stat := st.Sys().(*unix.Stat_t)
		if err := fs.unixFS.Chmod(staged, ufs.FileMode(stat.Mode&0o7777)); err != nil {
			return err
		}
		if stat.Nlink == 1 {
			removedSize = st.Size()
		}
	}
	if err := fs.unixFS.Replace(staged, p); err != nil {
		return err
	}
	fs.unixFS.Add(-removedSize)
	return nil
}

type streamWriter struct {
	fs   *Filesystem
	file ufs.File
}

func (w streamWriter) Write(p []byte) (int, error) {
	w.fs.mu.Lock()
	defer w.fs.mu.Unlock()
	if err := w.fs.HasSpaceFor(int64(len(p))); err != nil {
		return 0, err
	}
	n, err := w.file.Write(p)
	w.fs.unixFS.Add(int64(n))
	return n, err
}
