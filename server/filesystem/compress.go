package filesystem

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	iofs "io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/klauspost/compress/zip"
	"github.com/mholt/archives"
	"golang.org/x/sys/unix"

	"github.com/reviactyl/agent/internal/ufs"
)

// CompressFiles compresses all the files matching the given paths in the
// specified directory. This function also supports passing nested paths to only
// compress certain files and folders when working in a larger directory. This
// effectively creates a local backup, but rather than ignoring specific files
// and folders, it takes an allow-list of files and folders.
//
// All paths are relative to the dir that is passed in as the first argument,
// and the compressed file will be placed at that location named
// `archive-{date}.tar.gz`.
func (fs *Filesystem) CompressFiles(dir string, paths []string, format string) (ufs.FileInfo, error) {
	if format != "zip" && format != "tar.gz" {
		return nil, errors.New("unsupported archive format")
	}
	a := &Archive{Filesystem: fs, BaseDirectory: dir, Files: paths, Format: format}
	name := fmt.Sprintf("archive-%s", strings.ReplaceAll(time.Now().Format(time.RFC3339), ":", ""))
	var d string
	var f ufs.File
	var err error
	for i := 0; ; i++ {
		d = path.Join(dir, name+"."+format)
		if i > 0 {
			d = path.Join(dir, fmt.Sprintf("%s-%d.%s", name, i, format))
		}
		f, err = fs.unixFS.OpenFile(d, ufs.O_WRONLY|ufs.O_CREATE|ufs.O_EXCL, 0o644)
		if err == nil {
			break
		}
		if !errors.Is(err, ufs.ErrExist) || i >= 100 {
			return nil, err
		}
	}
	qf := newQuotaFile(fs, f, 0)
	removePartial := func() {
		_ = qf.Close()
		// Quota.Remove accounts for the removed file size itself. Do not also
		// adjust the cached usage here, or failed concurrent archives will
		// release their space twice and allow later writers to exceed the limit.
		_ = fs.unixFS.Remove(d)
	}
	if err := a.Stream(context.Background(), qf); err != nil {
		removePartial()
		return nil, err
	}
	st, err := qf.Stat()
	if err != nil {
		removePartial()
		return nil, err
	}
	if err := qf.Close(); err != nil {
		_ = fs.unixFS.Remove(d)
		return nil, err
	}
	return st, nil
}

// maxArchiveEntryNameLength is the longest supported archive entry path.
const maxArchiveEntryNameLength = 4096

// SpaceAvailableForDecompression looks through a given archive and determines
// if decompressing it would put the server over its allocated disk space limit.
func (fs *Filesystem) SpaceAvailableForDecompression(ctx context.Context, dir string, file string) error {
	if fs.MaxDisk() <= 0 {
		return nil
	}
	f, err := fs.unixFS.Open(filepath.Join(dir, file))
	if err != nil {
		return err
	}
	defer f.Close()

	format, _, err := archives.Identify(ctx, filepath.Base(file), f)
	if err != nil && !errors.Is(err, archives.NoMatch) {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}

	var size int64
	add := func(name string, n int64) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if len(name) > maxArchiveEntryNameLength {
			return errors.New("filesystem: archive contains an entry with a name that is too long")
		}
		if n <= 0 {
			return nil
		}
		destination := filepath.Join(dir, filepath.FromSlash(name))
		if err := fs.IsIgnored(destination); err != nil {
			return nil
		}
		existing, err := fs.unixFS.Stat(destination)
		if err != nil && !errors.Is(err, ufs.ErrNotExist) {
			return err
		}
		if err == nil {
			if existing.IsDir() {
				return newFilesystemError(ErrCodeIsDirectory, nil)
			}
			// Account only for growth: shrinking files cannot safely offset
			// other archive entries before extraction order is known.
			n = max(n-existing.Size(), 0)
		}
		if n > math.MaxInt64-size {
			return newFilesystemError(ErrCodeDiskSpace, nil)
		}
		size += n
		if !fs.unixFS.CanFit(size) {
			return newFilesystemError(ErrCodeDiskSpace, nil)
		}
		return nil
	}

	switch ff := format.(type) {
	case archives.Zip:
		reader, err := zip.NewReader(f, info.Size())
		if err != nil {
			return err
		}
		for _, entry := range reader.File {
			if entry.UncompressedSize64 > math.MaxInt64 {
				return newFilesystemError(ErrCodeDiskSpace, nil)
			}
			if err := add(entry.Name, int64(entry.UncompressedSize64)); err != nil {
				return err
			}
		}
		return nil
	case archives.Extraction:
		return ff.Extract(ctx, io.NewSectionReader(f, 0, info.Size()), func(_ context.Context, entry archives.FileInfo) error {
			return add(entry.NameInArchive, entry.Size())
		})
	case archives.Compression:
		name := strings.TrimSuffix(filepath.Base(file), ff.Extension())
		destination := filepath.Join(dir, name)
		if err := fs.IsIgnored(destination); err != nil {
			return nil
		}
		var existingSize int64
		if existing, err := fs.unixFS.Stat(destination); err == nil && existing.Mode().IsRegular() {
			existingSize = existing.Size()
		}
		reader, err := ff.OpenReader(f)
		if err != nil {
			return err
		}
		defer reader.Close()
		var total int64
		buf := make([]byte, 32*1024)
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, readErr := reader.Read(buf)
			if n > 0 {
				if int64(n) > math.MaxInt64-total {
					return newFilesystemError(ErrCodeDiskSpace, nil)
				}
				total += int64(n)
				if limit := fs.MaxDisk(); limit > 0 && total > max(limit, existingSize) {
					return newFilesystemError(ErrCodeDiskSpace, nil)
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return readErr
			}
		}
		return add(name, total)
	}
	return newFilesystemError(ErrCodeUnknownArchive, archives.NoMatch)
}

// DecompressFile will decompress a file in a given directory by using the
// archiver tool to infer the file type and go from there. This will walk over
// all the files within the given archive and ensure that there is not a
// zip-slip attack being attempted by validating that the final path is within
// the server data directory.
func (fs *Filesystem) DecompressFile(ctx context.Context, dir string, file string) error {
	f, err := fs.unixFS.Open(filepath.Join(dir, file))
	if err != nil {
		return err
	}
	defer f.Close()

	// Identify the type of archive we are dealing with.
	format, input, err := archives.Identify(ctx, filepath.Base(file), f)
	if err != nil {
		if errors.Is(err, archives.NoMatch) {
			return newFilesystemError(ErrCodeUnknownArchive, err)
		}
		return err
	}

	return fs.extractStream(ctx, extractStreamOptions{
		FileName:  file,
		Directory: dir,
		Format:    format,
		Reader:    input,
	})
}

// ExtractTransfer extracts the archive of a server that is being transferred to
// this node into the server's directory. Every file is kept, including ones the
// denylist would refuse, since they are the server's own files.
func (fs *Filesystem) ExtractTransfer(ctx context.Context, r io.Reader) error {
	format, input, err := archives.Identify(ctx, "archive.tar.gz", r)
	if err != nil {
		if errors.Is(err, archives.NoMatch) {
			return newFilesystemError(ErrCodeUnknownArchive, err)
		}
		return err
	}
	return fs.extractStream(ctx, extractStreamOptions{
		Directory:    "/",
		Format:       format,
		Reader:       input,
		KeepDenylist: true,
	})
}

type extractStreamOptions struct {
	// The directory to extract the archive to.
	Directory string
	// File name of the archive.
	FileName string
	// Format of the archive.
	Format archives.Format
	// Reader for the archive.
	Reader io.Reader
	// KeepDenylist extracts files the denylist would otherwise skip.
	KeepDenylist bool
}

func (fs *Filesystem) extractStream(ctx context.Context, opts extractStreamOptions) error {
	// See if it's a compressed archive, such as TAR or a ZIP
	ex, ok := opts.Format.(archives.Extractor)
	if !ok {
		// If not, check if it's a single-file compression, such as
		// .log.gz, .sql.gz, and so on
		de, ok := opts.Format.(archives.Decompressor)
		if !ok {
			return nil
		}

		// Strip the compression suffix
		p := filepath.Join(opts.Directory, strings.TrimSuffix(opts.FileName, opts.Format.Extension()))

		// Make sure it's not ignored
		if err := fs.IsIgnored(p); err != nil {
			return nil
		}

		reader, err := de.OpenReader(opts.Reader)
		if err != nil {
			return err
		}
		defer reader.Close()

		if _, err := fs.WriteFrom(p, reader); err != nil {
			return wrapError(err, opts.FileName)
		}
		return nil
	}

	// Decompress and extract archive
	extracted := make(map[string]struct{})
	return ex.Extract(ctx, opts.Reader, func(ctx context.Context, f archives.FileInfo) error {
		p := filepath.Join(opts.Directory, f.NameInArchive)
		delete(extracted, path.Clean(f.NameInArchive))
		// If it is ignored, just don't do anything with the entry and skip over it.
		if !opts.KeepDenylist {
			if err := fs.IsIgnored(p); err != nil {
				return nil
			}
		}
		// Create directories explicitly; an empty one has no file to create it
		// implicitly and would otherwise be dropped during extraction.
		if f.IsDir() {
			if err := fs.mkdirAll(p, 0o755); err != nil {
				return wrapError(err, opts.FileName)
			}
			return nil
		}
		if f.Mode()&iofs.ModeSymlink != 0 {
			if f.LinkTarget == "" {
				return nil
			}
			if err := fs.HasSpaceFor(0); err != nil {
				return err
			}
			if err := fs.mkdirAll(filepath.Dir(p), 0o755); err != nil {
				return wrapError(err, opts.FileName)
			}
			// Create the link itself, never open its target as a regular file.
			if err := fs.Symlink(f.LinkTarget, p); err != nil {
				if !errors.Is(err, ufs.ErrExist) {
					return wrapError(err, opts.FileName)
				}
				st, err := fs.unixFS.Lstat(p)
				if err != nil {
					return wrapError(err, opts.FileName)
				}
				if st.IsDir() {
					return newFilesystemError(ErrCodeIsDirectory, nil)
				}
				if err := fs.unixFS.Remove(p); err != nil {
					return wrapError(err, opts.FileName)
				}
				if err := fs.Symlink(f.LinkTarget, p); err != nil {
					return wrapError(err, opts.FileName)
				}
			}
			// Chown uses Lchown; do not apply Chtimes through the link.
			return fs.chownFile(p)
		}
		if !f.Mode().IsRegular() {
			return nil
		}
		if h, ok := f.Header.(*tar.Header); ok && h.Typeflag == tar.TypeLink {
			target := path.Clean(h.Linkname)
			if !opts.KeepDenylist {
				if err := fs.IsIgnored(filepath.Join(opts.Directory, target)); err != nil {
					return nil
				}
			}
			if _, ok := extracted[target]; !ok || target == path.Clean(f.NameInArchive) {
				return fmt.Errorf("invalid or unavailable TAR hard-link target %q", h.Linkname)
			}
			source, err := fs.unixFS.Lstat(filepath.Join(opts.Directory, target))
			if err != nil {
				return wrapError(err, opts.FileName)
			}
			if !source.Mode().IsRegular() {
				return fmt.Errorf("TAR hard-link target %q is not a regular file", h.Linkname)
			}
			if existing, err := fs.unixFS.Lstat(p); err == nil {
				if existing.IsDir() {
					return newFilesystemError(ErrCodeIsDirectory, nil)
				}
				if os.SameFile(source, existing) {
					extracted[path.Clean(f.NameInArchive)] = struct{}{}
					return nil
				}
			}
			if err := fs.HasSpaceFor(0); err != nil {
				return err
			}
			if err := fs.mkdirAll(filepath.Dir(p), 0o755); err != nil {
				return wrapError(err, opts.FileName)
			}
			temporary := filepath.Join(filepath.Dir(p), ".extract-"+rand.Text())
			if err := fs.unixFS.Link(filepath.Join(opts.Directory, target), temporary); err != nil {
				return wrapError(err, opts.FileName)
			}
			defer func() { _ = fs.unixFS.UnixFS.Remove(temporary) }()
			if err := func() error {
				fs.mu.Lock()
				defer fs.mu.Unlock()
				previous, err := fs.unixFS.Lstat(p)
				if err != nil && !errors.Is(err, ufs.ErrNotExist) {
					return err
				}
				credit, err := replacementCredit(fs, p)
				if err != nil {
					return err
				}
				if err := fs.unixFS.Replace(temporary, p); err != nil {
					return err
				}
				fs.forgetWalkFileLocked(previous)
				fs.addDisk(-credit)
				return nil
			}(); err != nil {
				return wrapError(err, opts.FileName)
			}
			extracted[path.Clean(f.NameInArchive)] = struct{}{}
			return nil
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		defer r.Close()
		var writeErr error
		if existing, err := fs.unixFS.Lstat(p); err == nil && existing.Mode().IsRegular() {
			if st, ok := existing.Sys().(*unix.Stat_t); ok && st.Nlink > 1 {
				_, writeErr = fs.writeStream(p, r, f.Mode(), true)
			} else {
				writeErr = fs.Write(p, r, f.Size(), f.Mode())
			}
		} else {
			writeErr = fs.Write(p, r, f.Size(), f.Mode())
		}
		if writeErr != nil {
			return wrapError(writeErr, opts.FileName)
		}
		// Update the file modification time to the one set in the archive.
		if err := fs.Chtimes(p, f.ModTime(), f.ModTime()); err != nil {
			return wrapError(err, opts.FileName)
		}
		extracted[path.Clean(f.NameInArchive)] = struct{}{}
		return nil
	})
}
