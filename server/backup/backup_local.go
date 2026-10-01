package backup

import (
	"context"
	"io"
	"os"

	"emperror.dev/errors"
	"github.com/juju/ratelimit"
	"github.com/mholt/archives"

	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/remote"
	"github.com/reviactyl/agent/server/filesystem"
)

type LocalBackup struct {
	Backup
}

var _ BackupInterface = (*LocalBackup)(nil)

func NewLocal(client remote.Client, uuid string, ignore string, archiveFormat ...string) *LocalBackup {
	selectedFormat := "tar.gz"
	if len(archiveFormat) > 0 {
		selectedFormat = archiveFormat[0]
	}
	return &LocalBackup{
		Backup{
			client:  client,
			Uuid:    uuid,
			Ignore:  ignore,
			Format:  selectedFormat,
			adapter: LocalBackupAdapter,
		},
	}
}

// LocateLocal finds the backup for a server and returns the local path. This
// will obviously only work if the backup was created as a local backup.
func LocateLocal(client remote.Client, uuid string, archiveFormat ...string) (*LocalBackup, os.FileInfo, error) {
	b := NewLocal(client, uuid, "", archiveFormat...)
	if err := b.validateIdentifier(); err != nil {
		return nil, nil, err
	}
	st, err := os.Stat(b.Path())
	if errors.Is(err, os.ErrNotExist) && len(archiveFormat) == 0 {
		b.Format = "zip"
		st, err = os.Stat(b.Path())
	}
	if err != nil {
		return nil, nil, err
	}

	if st.IsDir() {
		return nil, nil, errors.New("invalid archive, is directory")
	}

	return b, st, nil
}

// Remove removes a backup from the system.
func (b *LocalBackup) Remove() error {
	if err := b.validateIdentifier(); err != nil {
		return err
	}
	return os.Remove(b.Path())
}

// WithLogContext attaches additional context to the log output for this backup.
func (b *LocalBackup) WithLogContext(c map[string]interface{}) {
	b.logContext = c
}

// Generate generates a backup of the selected files and pushes it to the
// defined location for this instance.
func (b *LocalBackup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	if err := b.validateIdentifier(); err != nil {
		return nil, err
	}
	a := &filesystem.Archive{
		Filesystem: fsys,
		Ignore:     ignore,
		Format:     b.Format,
	}

	b.log().WithField("path", b.Path()).Info("creating backup for server")
	if err := a.Create(ctx, b.Path()); err != nil {
		return nil, err
	}
	b.log().Info("created backup successfully")

	ad, err := b.Details(ctx, nil)
	if err != nil {
		return nil, errors.WrapIf(err, "backup: failed to get archive details for local backup")
	}
	return ad, nil
}

// Restore will walk over the archive and call the callback function for each
// file encountered.
func (b *LocalBackup) Restore(ctx context.Context, _ io.Reader, callback RestoreCallback) error {
	if err := b.validateIdentifier(); err != nil {
		return err
	}
	f, err := os.Open(b.Path())
	if err != nil {
		return err
	}
	defer f.Close()

	var reader io.Reader = f
	var zipLimiter *ratelimit.Bucket
	// Steal the logic we use for making backups which will be applied when restoring
	// this specific backup. This allows us to prevent overloading the disk unintentionally.
	if writeLimit := int64(config.Get().System.Backups.WriteLimit * 1024 * 1024); writeLimit > 0 {
		limiter := ratelimit.NewBucketWithRate(float64(writeLimit), writeLimit)
		if b.Format == "zip" {
			zipLimiter = limiter
		} else {
			reader = ratelimit.Reader(f, limiter)
		}
	}
	if err := b.archiveFormat().Extract(ctx, reader, func(ctx context.Context, f archives.FileInfo) error {
		r, err := f.Open()
		if err != nil {
			return err
		}
		defer r.Close()
		var callbackReader io.ReadCloser = r
		if zipLimiter != nil {
			callbackReader = struct {
				io.Reader
				io.Closer
			}{ratelimit.Reader(r, zipLimiter), r}
		}

		return callback(f.NameInArchive, f.FileInfo, f.LinkTarget, callbackReader)
	}); err != nil {
		return err
	}
	return nil
}
