package filesystem

import (
	"io"
	"math"
	"sync"

	"github.com/reviactyl/agent/internal/ufs"
)

type quotaFile struct {
	ufs.File

	fs   *Filesystem
	mu   sync.Mutex
	size int64
}

func newQuotaFile(fs *Filesystem, file ufs.File, size int64) ufs.File {
	return &quotaFile{File: file, fs: fs, size: size}
}

func (f *quotaFile) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return f.File.Write(p)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	off, err := f.File.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}

	return f.writeAtLocked(p, off, func() (int, error) {
		return f.File.Write(p)
	})
}

func (f *quotaFile) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || len(p) == 0 {
		return f.File.WriteAt(p, off)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.writeAtLocked(p, off, func() (int, error) {
		return f.File.WriteAt(p, off)
	})
}

// CheckSize checks whether replacing this file's contents with a file of the
// given size would fit the disk limit, without changing the file.
func (f *quotaFile) CheckSize(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if size <= f.size {
		return nil
	}
	return f.fs.HasSpaceFor(size - f.size)
}

// ReplaceContents replaces the file's contents with data. Any growth is
// reserved before the file is modified. The original contents are left in
// place if there is not enough space, because the file is overwritten in
// place and only then truncated to the new length.
func (f *quotaFile) ReplaceContents(data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	previousSize := f.size
	newSize := int64(len(data))
	reserved := max(newSize-previousSize, 0)
	if reserved > 0 {
		if err := f.fs.reserveDisk(reserved); err != nil {
			return err
		}
	}

	if _, err := f.File.Seek(0, io.SeekStart); err != nil {
		if reserved > 0 {
			f.fs.releaseDisk(reserved, 0)
		}
		return err
	}

	f.fs.mu.Lock()
	n, err := f.File.Write(data)
	if n > 0 {
		f.fs.recordWalkFileSizeLocked(f.File)
	}
	f.fs.mu.Unlock()
	if int64(n) > f.size {
		f.size = int64(n)
	}
	used := max(f.size-previousSize, 0)

	if err != nil {
		if reserved > 0 {
			f.fs.releaseDisk(reserved, used)
		}
		return err
	}

	if err := f.File.Truncate(newSize); err != nil {
		if reserved > 0 {
			f.fs.releaseDisk(reserved, used)
		}
		return err
	}

	if newSize != f.size {
		f.fs.mu.Lock()
		f.fs.recordWalkFileSizeLocked(f.File)
		f.fs.mu.Unlock()
		f.fs.adjustDisk(newSize - f.size)
		f.size = newSize
	}
	if reserved > 0 {
		f.fs.releaseDisk(reserved, used)
	}
	return nil
}

func (f *quotaFile) writeAtLocked(p []byte, off int64, write func() (int, error)) (int, error) {
	previousSize := f.size
	end, ok := quotaWriteEnd(off, len(p))
	if !ok {
		return 0, newFilesystemError(ErrCodeDiskSpace, nil)
	}
	reserved := end - previousSize
	if reserved > 0 {
		if err := f.fs.reserveDisk(reserved); err != nil {
			return 0, err
		}
	}

	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	n, err := write()
	writtenEnd := previousSize
	if n > 0 {
		writtenEnd, _ = quotaWriteEnd(off, n)
		if writtenEnd > previousSize {
			f.size = writtenEnd
		}
		f.fs.recordWalkFileSizeLocked(f.File)
	}

	if reserved > 0 {
		actual := int64(0)
		if writtenEnd > previousSize {
			actual = writtenEnd - previousSize
		}
		f.fs.releaseDiskLocked(reserved, actual)
	}
	return n, err
}

func quotaWriteEnd(off int64, size int) (int64, bool) {
	if size < 0 || off > math.MaxInt64-int64(size) {
		return 0, false
	}
	return off + int64(size), true
}

func (f *quotaFile) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(quotaFileWriter{file: f}, r)
}

func (f *quotaFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	st, statErr := f.File.Stat()
	closeErr := f.File.Close()
	if statErr == nil {
		f.fs.adjustDisk(st.Size() - f.size)
	}
	if statErr != nil {
		return statErr
	}
	return closeErr
}

type quotaFileWriter struct {
	file *quotaFile
}

func (w quotaFileWriter) Write(p []byte) (int, error) {
	return w.file.Write(p)
}
