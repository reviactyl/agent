package filesystem

import (
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"golang.org/x/sys/unix"

	"github.com/reviactyl/agent/internal/ufs"
)

type SpaceCheckingOpts struct {
	AllowStaleResponse bool
}

// TODO: can this be replaced with some sort of atomic? Like atomic.Pointer?
type usageLookupTime struct {
	sync.RWMutex
	value time.Time
}

// Set sets the last time that a disk space lookup was performed.
func (ult *usageLookupTime) Set(t time.Time) {
	ult.Lock()
	ult.value = t
	ult.Unlock()
}

// Get the last time that we performed a disk space usage lookup.
func (ult *usageLookupTime) Get() time.Time {
	ult.RLock()
	defer ult.RUnlock()

	return ult.value
}

// MaxDisk returns the maximum amount of disk space that this Filesystem
// instance is allowed to use.
func (fs *Filesystem) MaxDisk() int64 {
	return fs.unixFS.Limit()
}

// SetDiskLimit sets the disk space limit for this Filesystem instance.
func (fs *Filesystem) SetDiskLimit(i int64) {
	fs.unixFS.SetLimit(i)
}

// The same concept as HasSpaceAvailable however this will return an error if there is
// no space, rather than a boolean value.
func (fs *Filesystem) HasSpaceErr(allowStaleValue bool) error {
	if !fs.HasSpaceAvailable(allowStaleValue) {
		return newFilesystemError(ErrCodeDiskSpace, nil)
	}
	return nil
}

// Determines if the directory a file is trying to be added to has enough space available
// for the file to be written to.
//
// Because determining the amount of space being used by a server is a taxing operation we
// will load it all up into a cache and pull from that as long as the key is not expired.
//
// This operation will potentially block unless allowStaleValue is set to true. See the
// documentation on DiskUsage for how this affects the call.
func (fs *Filesystem) HasSpaceAvailable(allowStaleValue bool) bool {
	size, err := fs.DiskUsage(allowStaleValue)
	if err != nil {
		log.WithField("root", fs.Path()).WithField("error", err).Warn("failed to determine root fs directory size")
	}

	// If space is -1 or 0 just return true, means they're allowed unlimited.
	//
	// Technically we could skip disk space calculation because we don't need to check if the
	// server exceeds its limit but because this method caches the disk usage it would be best
	// to calculate the disk usage and always return true.
	if fs.MaxDisk() == 0 {
		return true
	}

	return size <= fs.MaxDisk()
}

// Returns the cached value for the amount of disk space used by the filesystem. Do not rely on this
// function for critical logical checks. It should only be used in areas where the actual disk usage
// does not need to be perfect, e.g. API responses for server resource usage.
func (fs *Filesystem) CachedUsage() int64 {
	return fs.unixFS.Usage()
}

// Internal helper function to allow other parts of the codebase to check the total used disk space
// as needed without overly taxing the system. This will prioritize the value from the cache to avoid
// excessive IO usage. We will only walk the filesystem and determine the size of the directory if there
// is no longer a cached value.
//
// If "allowStaleValue" is set to true, a stale value MAY be returned to the caller if there is an
// expired cache value AND there is currently another lookup in progress. If there is no cached value but
// no other lookup is in progress, a fresh disk space response will be returned to the caller.
//
// This is primarily to avoid a bunch of I/O operations from piling up on the server, especially on servers
// with a large amount of files.
func (fs *Filesystem) DiskUsage(allowStaleValue bool) (int64, error) {
	// A disk check interval of 0 means this functionality is completely disabled.
	if fs.diskCheckInterval == 0 {
		return 0, nil
	}

	if !fs.lastLookupTime.Get().After(time.Now().Add(time.Second * fs.diskCheckInterval * -1)) {
		// If we are now allowing a stale response go ahead  and perform the lookup and return the fresh
		// value. This is a blocking operation to the calling process.
		if !allowStaleValue {
			return fs.refreshDiskUsage()
		} else if fs.lookupInProgress.CompareAndSwap(false, true) {
			// Otherwise, if we allow a stale value and there isn't a valid item in the cache and we aren't
			// currently performing a lookup, just do the disk usage calculation in the background.
			go func(fs *Filesystem) {
				defer fs.lookupInProgress.Store(false)
				if _, err := fs.refreshDiskUsage(); err != nil {
					log.WithField("root", fs.Path()).WithField("error", err).Warn("failed to update fs disk usage from within routine")
				}
			}(fs)
		}
	}

	// Return the currently cached value back to the calling function.
	return fs.unixFS.Usage(), nil
}

// refreshDiskUsage updates the currently used disk space for a server, unless
// another lookup finished while this one was waiting for it.
func (fs *Filesystem) refreshDiskUsage() (int64, error) {
	fs.lookupMu.Lock()
	defer fs.lookupMu.Unlock()

	if fs.lastLookupTime.Get().After(time.Now().Add(time.Second * fs.diskCheckInterval * -1)) {
		return fs.unixFS.Usage(), nil
	}
	return fs.walkDiskUsage()
}

// Updates the currently used disk space for a server.
func (fs *Filesystem) updateCachedDiskUsage() (int64, error) {
	// Obtain an exclusive lock on this process so that we don't unintentionally run it at the same
	// time as another running process.
	fs.lookupMu.Lock()
	defer fs.lookupMu.Unlock()

	return fs.walkDiskUsage()
}

// walkDiskUsage walks the disk and updates the cached usage. The caller must
// hold lookupMu.
func (fs *Filesystem) walkDiskUsage() (int64, error) {
	// Signal that we're currently updating the disk size so that other calls to the disk checking
	// functions can determine if they should queue up additional calls to this function. Ensure that
	// we always set this back to "false" when this process is done executing.
	fs.lookupInProgress.Store(true)
	defer fs.lookupInProgress.Store(false)

	// Writes are not blocked while walking the directory, so keep track of the space reserved for
	// writes that are in progress at any point during the walk.
	fs.mu.Lock()
	fs.walking = true
	fs.walkReserved = fs.reserved
	fs.walkCompleted = 0
	fs.walkCompletedDelta = 0
	fs.walkObserved = make(map[uint64]int64)
	fs.walkUnobserved = make(map[uint64]int64)
	fs.mu.Unlock()

	// One pass is sufficient: concurrent managed writes are reconciled against
	// the inode observations made by this pass. In particular, changed inodes
	// whose parent has already been visited remain in walkUnobserved.
	size, err := fs.directorySize("/", true)
	fs.mu.Lock()

	// Always record the lookup, even if there is an error, so that a persistent error does not
	// cause the disk to be walked on every call.
	fs.lastLookupTime.Set(time.Now())

	// Include active reservations and managed changes made after an inode was
	// observed. A partial walk cannot lower cached usage because its total is
	// incomplete; a successful walk is free to reclaim externally removed data.
	usage := size + fs.walkReserved + fs.walkCompletedDelta
	for _, unobservedSize := range fs.walkUnobserved {
		usage += unobservedSize
	}
	if err != nil {
		usage = max(usage, fs.unixFS.Usage())
	}
	fs.unixFS.SetUsage(usage)
	fs.walking = false
	fs.walkReserved = 0
	fs.walkCompleted = 0
	fs.walkCompletedDelta = 0
	fs.walkObserved = nil
	fs.walkUnobserved = nil
	fs.mu.Unlock()

	return usage, err
}

// DirectorySize calculates the size of a directory and its descendants.
func (fs *Filesystem) DirectorySize(root string) (int64, error) {
	return fs.directorySize(root, false)
}

func (fs *Filesystem) directorySize(root string, trackWrites bool) (int64, error) {
	dirfd, name, closeFd, err := fs.unixFS.SafePath(root)
	defer closeFd()
	if err != nil {
		return 0, err
	}

	var hardLinks []uint64

	var size atomic.Int64
	err = fs.unixFS.WalkDirat(dirfd, name, func(dirfd int, name, _ string, d ufs.DirEntry, err error) error {
		// Files can be removed while the walk is running, which is not an error.
		if errors.Is(err, ufs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return errors.Wrap(err, "walkdirat err")
		}

		// Only calculate the size of regular files.
		if !d.Type().IsRegular() {
			return nil
		}

		if trackWrites {
			fs.mu.Lock()
		}
		info, err := fs.unixFS.Lstatat(dirfd, name)
		if errors.Is(err, ufs.ErrNotExist) {
			if trackWrites {
				fs.mu.Unlock()
			}
			return nil
		}
		if err != nil {
			if trackWrites {
				fs.mu.Unlock()
			}
			return errors.Wrap(err, "lstatat err")
		}

		var sysFileInfo = info.Sys().(*unix.Stat_t)
		if sysFileInfo.Nlink > 1 {
			// Hard links have the same inode number
			if slices.Contains(hardLinks, sysFileInfo.Ino) {
				// Don't add hard links size twice
				if trackWrites {
					fs.mu.Unlock()
				}
				return nil
			} else {
				hardLinks = append(hardLinks, sysFileInfo.Ino)
			}
		}
		if trackWrites && fs.walking && fs.walkObserved != nil {
			previousSize, seen := fs.walkObserved[sysFileInfo.Ino]
			fs.walkObserved[sysFileInfo.Ino] = info.Size()
			delete(fs.walkUnobserved, sysFileInfo.Ino)
			if seen {
				fs.walkCompletedDelta += info.Size() - previousSize
				fs.mu.Unlock()
				return nil
			}
		}
		if trackWrites {
			fs.mu.Unlock()
		}

		size.Add(info.Size())
		if trackWrites && fs.walkEntryHook != nil {
			fs.walkEntryHook()
		}
		return nil
	})
	return size.Load(), errors.WrapIf(err, "server/filesystem: directorysize: failed to walk directory")
}

// fileInode returns the inode for info from either a UnixFS walk result or a
// real os.File handle. os.File.Stat returns *syscall.Stat_t, while Lstatat
// returns *unix.Stat_t.
func fileInode(info ufs.FileInfo) (uint64, bool) {
	switch st := info.Sys().(type) {
	case *unix.Stat_t:
		return st.Ino, true
	case *syscall.Stat_t:
		return st.Ino, true
	default:
		return 0, false
	}
}

// recordWalkFileSizeLocked adjusts the current walk when a managed write
// changes a file. The caller must hold fs.mu.
func (fs *Filesystem) recordWalkFileSizeLocked(file ufs.File) {
	if !fs.walking || fs.walkObserved == nil {
		return
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	ino, ok := fileInode(info)
	if !ok {
		var st unix.Stat_t
		if err := unix.Fstat(int(file.Fd()), &st); err != nil {
			return
		}
		ino = st.Ino
	}
	previousSize, seen := fs.walkObserved[ino]
	if !seen {
		fs.walkUnobserved[ino] = info.Size()
		return
	}
	fs.walkCompletedDelta += info.Size() - previousSize
	fs.walkObserved[ino] = info.Size()
}

func (fs *Filesystem) forgetWalkFileLocked(info ufs.FileInfo) {
	if !fs.walking || info == nil || !info.Mode().IsRegular() {
		return
	}
	st, ok := info.Sys().(*unix.Stat_t)
	if !ok || st.Nlink != 1 {
		return
	}
	if size, seen := fs.walkObserved[st.Ino]; seen {
		fs.walkCompletedDelta -= size
		delete(fs.walkObserved, st.Ino)
	}
	delete(fs.walkUnobserved, st.Ino)
}

type lockedFilesystemWriter struct {
	fs   *Filesystem
	file ufs.File
}

func (w lockedFilesystemWriter) Write(p []byte) (int, error) {
	w.fs.mu.Lock()
	defer w.fs.mu.Unlock()
	n, err := w.file.Write(p)
	if n > 0 {
		w.fs.recordWalkFileSizeLocked(w.file)
	}
	return n, err
}

func (fs *Filesystem) HasSpaceFor(size int64) error {
	if !fs.unixFS.CanFit(size) {
		return newFilesystemError(ErrCodeDiskSpace, nil)
	}
	return nil
}

// reserveDisk reserves space for a write that is about to happen, returning an
// error if there is not enough space available. Writes happening at the same time
// therefore cannot each use the space that is left. Every reservation must be
// followed by a call to releaseDisk once the write is finished.
func (fs *Filesystem) reserveDisk(size int64) error {
	if size <= 0 {
		return nil
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	if err := fs.HasSpaceFor(size); err != nil {
		return err
	}
	fs.unixFS.Add(size)
	fs.reserved += size
	if fs.walking {
		fs.walkReserved += size
	}
	return nil
}

// releaseDisk finishes a write that reserved space with reserveDisk, replacing
// the reserved space with the amount the write actually changed the disk usage
// by.
func (fs *Filesystem) releaseDisk(reserved int64, used int64) {
	if reserved < 0 {
		reserved = 0
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.releaseDiskLocked(reserved, used)
}

// releaseDiskLocked is releaseDisk with fs.mu already held.
func (fs *Filesystem) releaseDiskLocked(reserved int64, used int64) {
	if reserved < 0 {
		reserved = 0
	}
	fs.reserved -= reserved
	if fs.walking {
		fs.walkReserved = max(fs.walkReserved-reserved, 0)
		fs.walkCompleted++
	}
	fs.unixFS.Add(used - reserved)
}

func (fs *Filesystem) adjustDisk(size int64) int64 {
	if size == 0 {
		return fs.CachedUsage()
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.walking {
		fs.walkCompleted++
	}

	return fs.unixFS.Add(size)
}

// addDisk updates cached usage after a write whose size was already checked.
func (fs *Filesystem) addDisk(size int64) int64 {
	if fs.walking && size != 0 {
		fs.walkCompleted++
	}
	return fs.unixFS.Add(size)
}
