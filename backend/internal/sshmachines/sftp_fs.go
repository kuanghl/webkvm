package sshmachines

import (
	"io"
	"os"
	"time"

	"github.com/pkg/sftp"
)

// sftpFS is the slice of *sftp.Client the transfer engine actually uses.
// Naming it as an interface is what makes recursive transfers testable:
// TransferManager used to hold a concrete *SFTPSessionRegistry, so
// runUpload/runDownload had no coverage and a fake filesystem could not
// be stood up in a unit test.
//
// Every method exists on *sftp.Client in pkg/sftp v1.13.12. OpenFile
// takes no perm argument there, which is why putFile creates the file
// and then applies Chmod explicitly: the server-side umask would
// otherwise eat the bits.
type sftpFS interface {
	Stat(p string) (os.FileInfo, error)
	Lstat(p string) (os.FileInfo, error)
	ReadDir(p string) ([]os.FileInfo, error)
	Open(p string) (remoteFile, error)
	OpenFile(p string, flags int) (remoteFile, error)
	Create(p string) (remoteFile, error)
	Mkdir(p string) error
	MkdirAll(p string) error
	Remove(p string) error
	RemoveDirectory(p string) error
	RemoveAll(p string) error
	Rename(oldPath, newPath string) error
	PosixRename(oldPath, newPath string) error
	Chmod(p string, mode os.FileMode) error
	Chtimes(p string, atime, mtime time.Time) error
	Symlink(oldName, newName string) error
	ReadLink(p string) (string, error)
	HasExtension(ext string) (string, bool)
}

// remoteFile is the handle surface the engine needs. *sftp.File
// satisfies it; a test fake can wrap a bytes.Reader.
type remoteFile interface {
	io.Reader
	io.Writer
	io.Closer
	Stat() (os.FileInfo, error)
}

// sessionSource hands out a live filesystem for a machine together with
// a release func. The release matters: a long directory walk holds the
// session for far longer than the registry's idle timeout, so without a
// lease the GC would close the client out from under a running job.
type sessionSource interface {
	Acquire(machineID string) (sftpFS, func(), error)
	// Drop closes whatever session the machine has pooled, forcing every
	// request in flight on it to fail. Only a wedged, already-canceled job
	// justifies it: see TransferManager.watchdog.
	Drop(machineID string)
}

// sftpBridge narrows *sftp.Client's handle returns to remoteFile. Only
// the three openers need wrapping; everything else is promoted.
type sftpBridge struct{ *sftp.Client }

func (b sftpBridge) Open(p string) (remoteFile, error) {
	f, err := b.Client.Open(p)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (b sftpBridge) OpenFile(p string, flags int) (remoteFile, error) {
	f, err := b.Client.OpenFile(p, flags)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (b sftpBridge) Create(p string) (remoteFile, error) {
	f, err := b.Client.Create(p)
	if err != nil {
		return nil, err
	}
	return f, nil
}

var (
	_ sftpFS        = sftpBridge{}
	_ sessionSource = (*SFTPSessionRegistry)(nil)
)
