//go:build unix

package ufs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

type privateFileID struct{ device, inode uint64 }

var privateDirectories = struct {
	sync.RWMutex
	entries map[privateFileID]uint64
}{}

var errPrivateDirectory = fmt.Errorf("private staging directory: %w", ErrPermission)

// ProtectDirectory excludes an open staging directory across sandbox instances
// until released, including after an external rename.
func (fs *UnixFS) ProtectDirectory(fd int) (func(), error) {
	privateDirectories.Lock()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		privateDirectories.Unlock()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		privateDirectories.Unlock()
		return nil, unix.ENOTDIR
	}
	if st.Uid != uint32(os.Geteuid()) || st.Mode&0o077 != 0 {
		privateDirectories.Unlock()
		return nil, ErrPermission
	}
	id := privateFileID{st.Dev, st.Ino}
	if privateDirectories.entries == nil {
		privateDirectories.entries = make(map[privateFileID]uint64)
	}
	privateDirectories.entries[id]++
	privateDirectories.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			privateDirectories.Lock()
			privateDirectories.entries[id]--
			if privateDirectories.entries[id] == 0 {
				delete(privateDirectories.entries, id)
			}
			privateDirectories.Unlock()
		})
	}, nil
}

func (fs *UnixFS) checkPrivateFDLocked(fd int) error {
	if len(privateDirectories.entries) == 0 || fd < 0 {
		return nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if _, private := privateDirectories.entries[privateFileID{st.Dev, st.Ino}]; private {
		return errPrivateDirectory
	}
	return nil
}

func (fs *UnixFS) checkPrivateAtLocked(dirfd int, name string, flags int) error {
	if len(privateDirectories.entries) == 0 {
		return nil
	}
	if !filepath.IsAbs(name) {
		if err := fs.checkPrivateFDLocked(dirfd); err != nil {
			return err
		}
	}
	if parent := filepath.Dir(name); parent != "." {
		fd, err := unix.Openat(dirfd, parent, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err == nil {
			err = fs.checkPrivateFDLocked(fd)
			_ = unix.Close(fd)
		}
		if err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENOTDIR) {
			return err
		}
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, flags); err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return nil
		}
		return err
	}
	if _, private := privateDirectories.entries[privateFileID{st.Dev, st.Ino}]; private {
		return errPrivateDirectory
	}
	return nil
}

func (fs *UnixFS) checkPrivateAt(dirfd int, name string) error {
	privateDirectories.RLock()
	defer privateDirectories.RUnlock()
	return fs.checkPrivateAtLocked(dirfd, name, unix.AT_SYMLINK_NOFOLLOW)
}
