package filesystem

import (
	"errors"
	"io"
	"path/filepath"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/reviactyl/agent/internal/ufs"
)

func (fs *Filesystem) writeStream(p string, r io.Reader, mode ufs.FileMode, replace bool) (_ int64, err error) {
	if fs.MaxDisk() == -1 {
		return 0, newFilesystemError(ErrCodeDiskSpace, nil)
	}
	if err := fs.mkdirAll(filepath.Dir(p), 0o755); err != nil {
		return 0, err
	}
	var credit int64
	if replace {
		if st, statErr := fs.unixFS.Lstat(p); statErr == nil {
			if st.Mode().IsRegular() {
				if stat, ok := st.Sys().(*unix.Stat_t); ok && stat.Nlink == 1 {
					credit = st.Size()
				}
			}
		} else if !errors.Is(statErr, ufs.ErrNotExist) {
			return 0, statErr
		}
	}
	staged := filepath.Join(filepath.Dir(p), ".download-"+uuid.NewString())
	file, err := fs.unixFS.OpenFile(staged, ufs.O_WRONLY|ufs.O_CREATE|ufs.O_EXCL, mode)
	if err != nil {
		return 0, err
	}
	defer func() {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		if cleanupErr := fs.unixFS.Remove(staged); !errors.Is(cleanupErr, ufs.ErrNotExist) {
			err = errors.Join(err, cleanupErr)
		}
	}()
	n, copyErr := io.Copy(streamWriter{fs: fs, file: file, credit: credit}, r)
	closeErr := file.Close()
	if copyErr != nil {
		return n, copyErr
	}
	if closeErr != nil {
		return n, closeErr
	}
	if err := fs.chownFile(staged); err != nil {
		return n, err
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	var removedSize int64
	st, err := fs.unixFS.Lstat(p)
	if err != nil && !errors.Is(err, ufs.ErrNotExist) {
		return n, err
	}
	if err == nil && st.Mode().IsRegular() {
		stat := st.Sys().(*unix.Stat_t)
		if err := fs.unixFS.Chmod(staged, ufs.FileMode(stat.Mode&0o7777)); err != nil {
			return n, err
		}
		if stat.Nlink == 1 {
			removedSize = st.Size()
		}
	}
	if err := fs.unixFS.Replace(staged, p); err != nil {
		return n, err
	}
	fs.addDisk(-removedSize)
	return n, nil
}

type streamWriter struct {
	fs     *Filesystem
	file   ufs.File
	credit int64
}

func (w streamWriter) Write(p []byte) (int, error) {
	w.fs.mu.Lock()
	defer w.fs.mu.Unlock()
	limit := w.fs.MaxDisk()
	usage := max(w.fs.CachedUsage()-w.credit, 0)
	if limit == -1 || (limit > 0 && (usage >= limit || int64(len(p)) > limit-usage)) {
		return 0, newFilesystemError(ErrCodeDiskSpace, nil)
	}
	n, err := w.file.Write(p)
	w.fs.addDisk(int64(n))
	return n, err
}
