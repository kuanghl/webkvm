package sshmachines

// Atomic destination writes: recursive transfers, machine-to-machine
// copies, single-file uploads and the inline SFTP write endpoint all go
// through here, so no remote file is ever left half-written by a failed
// or cancelled job.
//
// ".part then rename" is the only pattern that holds on plain SFTP: the
// destination keeps its previous contents until the last byte has landed,
// and a failure removes the scratch file instead of stranding it.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/sftp"
)

// partSuffix is spliced into the destination name while a file is being
// written. It carries the job id so two concurrent jobs can never share
// a scratch name.
func partSuffix(jobID string) string { return ".webkvm-part-" + jobID }

// putOpts tunes one atomic destination write.
type putOpts struct {
	JobID   string
	Mode    os.FileMode // 0 = leave the server's default (umask-applied)
	ModTime time.Time   // zero = leave now()
	OnBytes func(delta int64)
	// Gate parks the writer while a job is paused. It is consulted once
	// per chunk, before the read, so a pause takes effect within one
	// 64 KiB chunk; a non-nil error stops the write and the scratch
	// file is removed by the caller.
	Gate func(ctx context.Context) error
}

// putStream writes src to finalPath through a job-private scratch file,
// then flips it into place. Any failure removes the scratch file, so a
// cancelled or broken transfer leaves nothing behind but the original
// destination, untouched.
func putStream(ctx context.Context, dst sftpFS, finalPath string, src io.Reader, o putOpts) error {
	tmp := finalPath + partSuffix(o.JobID)
	f, err := dst.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	copyErr := stream(ctx, f, src, o)
	if cerr := f.Close(); copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil {
		_ = dst.Remove(tmp)
		return copyErr
	}
	if o.Mode != 0 {
		// After the write: OpenFile carries no perm argument and the
		// server applies its umask, so the mode has to be asserted here.
		if err := dst.Chmod(tmp, o.Mode.Perm()); err != nil {
			_ = dst.Remove(tmp)
			return fmt.Errorf("chmod %s: %w", tmp, err)
		}
	}
	if !o.ModTime.IsZero() {
		// Best effort: mtime is cosmetic on a copy, and a server that
		// refuses it must not fail a whole tree.
		_ = dst.Chtimes(tmp, o.ModTime, o.ModTime)
	}
	if err := renameAtomic(dst, tmp, finalPath); err != nil {
		_ = dst.Remove(tmp)
		return fmt.Errorf("publish %s: %w", finalPath, err)
	}
	return nil
}

// stream copies with a cancellation check every chunk. It cannot use
// io.Copy because a stalled remote read would then be uninterruptible
// and Cancel() would only take effect at EOF.
func stream(ctx context.Context, w io.Writer, r io.Reader, o putOpts) error {
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if o.Gate != nil {
			if err := o.Gate(ctx); err != nil {
				return err
			}
		}
		n, rerr := r.Read(buf)
		if n > 0 {
			written, werr := w.Write(buf[:n])
			if werr != nil {
				return werr
			}
			// pkg/sftp reports a short write as success; without this
			// check the scratch file would be renamed into place with
			// the tail missing and no error anywhere.
			if written != n {
				return fmt.Errorf("short write: %d of %d bytes", written, n)
			}
			if o.OnBytes != nil {
				o.OnBytes(int64(n))
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			return rerr
		}
	}
}

// renameAtomic replaces finalPath with tmp. OpenSSH exposes the
// posix-rename extension for exactly this; elsewhere SFTP has no
// replace-rename, so the fallback unlinks first and accepts the tiny
// window where the destination is missing.
func renameAtomic(fs sftpFS, tmp, final string) error {
	if _, ok := fs.HasExtension("posix-rename@openssh.com"); ok {
		if err := fs.PosixRename(tmp, final); err == nil {
			return nil
		}
	}
	if err := fs.Rename(tmp, final); err == nil {
		return nil
	} else if rmErr := fs.Remove(final); rmErr != nil {
		return err
	} else {
		return fs.Rename(tmp, final)
	}
}

// Overwrite policies for copy jobs.
const (
	OverwriteSkip      = "skip"
	OverwriteOverwrite = "overwrite"
	OverwriteRename    = "rename"
)

// normalizeOverwrite maps anything unknown to "overwrite", which is the
// least surprising result for a user who just dragged a folder onto a
// machine that already has it.
func normalizeOverwrite(p string) string {
	switch p {
	case OverwriteSkip, OverwriteRename:
		return p
	default:
		return OverwriteOverwrite
	}
}

// conflictAct is what the overwrite policy decided about one existing
// destination.
type conflictAct int

const (
	conflictWrite conflictAct = iota
	conflictSkip
)

// resolveConflict turns (finalPath, policy) into the path to actually
// write, or a decision to skip. Existence is probed on the destination
// rather than tracked locally, so two jobs copying into the same
// directory cannot mint the same " (1)" name.
//
// The probe is an Lstat: the rename below replaces whatever sits at the
// destination path itself, so that is the thing whose existence matters.
// Following a symlink here would call a dangling link "free" and let a
// "skip" policy walk straight onto it.
func resolveConflict(fs sftpFS, final, policy string) (string, conflictAct) {
	if _, err := fs.Lstat(final); err != nil {
		// Missing (or momentarily unreadable): write, and let the
		// transfer surface a real error if there is one.
		return final, conflictWrite
	}
	switch policy {
	case OverwriteSkip:
		return final, conflictSkip
	case OverwriteRename:
		return freeName(fs, final), conflictWrite
	default:
		return final, conflictWrite
	}
}

// freeName finds "<base> (n)<ext>" that the destination does not have.
// A leading dot belongs to the name, not the extension: ".bashrc (1)",
// never " (1).bashrc". Lstat for the same reason resolveConflict uses it:
// whatever occupies the path, link or not, is what the rename would eat.
func freeName(fs sftpFS, final string) string {
	dir, base := path.Split(final)
	stem, ext := base, ""
	if !strings.HasPrefix(base, ".") {
		ext = path.Ext(base)
		stem = strings.TrimSuffix(base, ext)
	}
	for i := 1; i <= 1000; i++ {
		cand := dir + fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if _, err := fs.Lstat(cand); err != nil {
			return cand
		}
	}
	return final
}

// PutInline publishes an in-memory payload to finalPath through the same
// scratch-then-rename transaction the transfer jobs use. The inline SFTP
// write endpoint used to Create+Write directly, which truncated a good
// remote file and left it half-full the moment the connection dropped
// mid-write.
func PutInline(client *sftp.Client, finalPath string, data []byte, mode os.FileMode) error {
	return putStream(context.Background(), sftpBridge{client}, finalPath, bytes.NewReader(data), putOpts{
		JobID: uuid.NewString(),
		Mode:  mode,
	})
}
