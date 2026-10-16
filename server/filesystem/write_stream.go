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
	creditPath := ""
	if replace {
		creditPath = p
	}
	return fs.writeStreamWithCredit(p, r, mode, creditPath)
}

func (fs *Filesystem) writeStreamWithCredit(p string, r io.Reader, mode ufs.FileMode, creditPath string) (_ int64, err error) {
	if fs.MaxDisk() == -1 {
		return 0, newFilesystemError(ErrCodeDiskSpace, nil)
	}
	if err := fs.mkdirAll(filepath.Dir(p), 0o755); err != nil {
		return 0, err
	}
	var credit int64
	if creditPath != "" {
		var creditErr error
		credit, creditErr = replacementCredit(fs, creditPath)
		if creditErr != nil {
			return 0, creditErr
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
		var stagedSize int64
		if st, statErr := fs.unixFS.Lstat(staged); statErr == nil && st.Mode().IsRegular() {
			stagedSize = st.Size()
		}
		if cleanupErr := fs.unixFS.Remove(staged); !errors.Is(cleanupErr, ufs.ErrNotExist) {
			err = errors.Join(err, cleanupErr)
		} else if stagedSize > 0 {
			fs.addDisk(-stagedSize)
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
	projectedRemoved := removedSize
	if creditPath != "" && creditPath != p {
		credit, err = replacementCredit(fs, creditPath)
		if err != nil {
			return n, err
		}
		projectedRemoved += credit
	}
	// The destination may have been deleted or changed while the staged write
	// was in progress, invalidating the credit used by streamWriter. Check the
	// actual resulting usage before committing the replacement.
	limit := fs.MaxDisk()
	if limit > 0 && fs.CachedUsage()-projectedRemoved > limit {
		return n, newFilesystemError(ErrCodeDiskSpace, nil)
	}
	if err := fs.unixFS.Replace(staged, p); err != nil {
		return n, err
	}
	fs.addDisk(-removedSize)
	return n, nil
}

// replacementCredit returns space that a successful replacement can reclaim.
// Files with multiple hard links cannot be credited because replacing one link
// does not free the inode's data.
func replacementCredit(fs *Filesystem, p string) (int64, error) {
	st, err := fs.unixFS.Lstat(p)
	if errors.Is(err, ufs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !st.Mode().IsRegular() {
		return 0, nil
	}
	stat, ok := st.Sys().(*unix.Stat_t)
	if !ok || stat.Nlink != 1 {
		return 0, nil
	}
	return st.Size(), nil
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
