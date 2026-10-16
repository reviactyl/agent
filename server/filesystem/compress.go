package filesystem

import (
	"context"
	"fmt"
	"io"
	iofs "io/fs"
	"math"
	"path"
	"path/filepath"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/klauspost/compress/zip"
	"github.com/mholt/archives"

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
	removePartial := func(st ufs.FileInfo) {
		if st == nil {
			st, _ = qf.Stat()
		}
		_ = qf.Close()
		if st == nil {
			st, _ = fs.unixFS.Stat(d)
		}
		_ = fs.unixFS.Remove(d)
		if st != nil {
			fs.adjustDisk(-st.Size())
		}
	}
	if err := a.Stream(context.Background(), qf); err != nil {
		removePartial(nil)
		return nil, err
	}
	st, err := qf.Stat()
	if err != nil {
		removePartial(nil)
		return nil, err
	}
	if err := qf.Close(); err != nil {
		_ = fs.unixFS.Remove(d)
		fs.adjustDisk(-st.Size())
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
		reader, err := ff.OpenReader(f)
		if err != nil {
			return err
		}
		defer reader.Close()
		var total int64
		buf := make([]byte, 32*1024)
		for {
			n, readErr := reader.Read(buf)
			if n > 0 {
				if int64(n) > math.MaxInt64-total {
					return newFilesystemError(ErrCodeDiskSpace, nil)
				}
				total += int64(n)
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return readErr
			}
		}
		name := strings.TrimSuffix(filepath.Base(file), ff.Extension())
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

// ExtractStreamUnsafe .
func (fs *Filesystem) ExtractStreamUnsafe(ctx context.Context, dir string, r io.Reader) error {
	format, input, err := archives.Identify(ctx, "archive.tar.gz", r)
	if err != nil {
		if errors.Is(err, archives.NoMatch) {
			return newFilesystemError(ErrCodeUnknownArchive, err)
		}
		return err
	}
	return fs.extractStream(ctx, extractStreamOptions{
		Directory: dir,
		Format:    format,
		Reader:    input,
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

		// Open the file for creation/writing
		f, err := fs.unixFS.OpenFile(p, ufs.O_WRONLY|ufs.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()

		st, err := f.Stat()
		if err != nil {
			return err
		}
		// Check the first chunk before replacing the destination, accounting
		// for the space that truncation will release.
		buf := make([]byte, 4096)
		var n int
		var readErr error
		for n == 0 && readErr == nil {
			n, readErr = reader.Read(buf)
		}
		if n == 0 && readErr != io.EOF {
			return readErr
		}
		// Truncation is a write even when the decompressed stream is empty.
		if quotaErr := fs.HasSpaceFor(0); quotaErr != nil {
			return quotaErr
		}
		usageAfterTruncation := max(fs.CachedUsage()-st.Size(), 0)
		if limit := fs.MaxDisk(); n > 0 && limit > 0 &&
			(usageAfterTruncation >= limit || int64(n) > limit-usageAfterTruncation) {
			return newFilesystemError(ErrCodeDiskSpace, nil)
		}
		// Replace existing contents and release their quota before writing.
		if err := f.Truncate(0); err != nil {
			return err
		}
		fs.addDisk(-st.Size())

		// Read in 4 KB chunks
		for {
			if n > 0 {

				// Check quota before writing the chunk
				if quotaErr := fs.HasSpaceFor(int64(n)); quotaErr != nil {
					return quotaErr
				}

				// Write the chunk
				if _, writeErr := f.Write(buf[:n]); writeErr != nil {
					return writeErr
				}

				// Add to quota
				fs.addDisk(int64(n))
			}

			if readErr != nil {
				// EOF are expected
				if readErr == io.EOF {
					break
				}

				// Return any other
				return readErr
			}
			n, readErr = reader.Read(buf)
		}

		return nil
	}

	// Decompress and extract archive
	return ex.Extract(ctx, opts.Reader, func(ctx context.Context, f archives.FileInfo) error {
		p := filepath.Join(opts.Directory, f.NameInArchive)
		// If it is ignored, just don't do anything with the entry and skip over it.
		if err := fs.IsIgnored(p); err != nil {
			return nil
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
		r, err := f.Open()
		if err != nil {
			return err
		}
		defer r.Close()
		if err := fs.Write(p, r, f.Size(), f.Mode()); err != nil {
			return wrapError(err, opts.FileName)
		}
		// Update the file modification time to the one set in the archive.
		if err := fs.Chtimes(p, f.ModTime(), f.ModTime()); err != nil {
			return wrapError(err, opts.FileName)
		}
		return nil
	})
}
