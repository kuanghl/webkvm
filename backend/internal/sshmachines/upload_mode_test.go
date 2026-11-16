package sshmachines

// The SFTP subsystem takes no permission on create, so an upload that
// asks for a mode has to follow the write with a chmod, and one that
// does not ask must leave the server's own default alone. runUpload had
// no coverage at all before these: it could only be driven against a
// live sshd, which is exactly what the sftpFS seam removed.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func uploadFixture(t *testing.T) (*TransferManager, *memFS, string, string) {
	t.Helper()
	m, src, alpha, _ := copyFixture(t)
	fs := src.fsFor(alpha)
	// The destination's parent has to exist for runUpload's pre-flight
	// check, and one sibling file keeps the tree from being trivial.
	fs.writeFile(t, "/srv/keep.txt", "keep", 0o600)
	local := filepath.Join(t.TempDir(), "deploy.sh")
	if err := os.WriteFile(local, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatalf("stage local: %v", err)
	}
	return m, fs, alpha, local
}

func TestUploadAppliesTheRequestedMode(t *testing.T) {
	m, fs, alpha, local := uploadFixture(t)
	// An existing destination exercises the backup sidecar next to the
	// new chmod: the sidecar must still be gone once both succeeded.
	fs.writeFile(t, "/srv/deploy.sh", "old", 0o644)

	job, err := m.StartUpload(alpha, "/srv/deploy.sh", local, 0o755)
	if err != nil {
		t.Fatalf("start upload: %v", err)
	}
	tr := waitJob(t, m, job.ID)
	mustDone(t, tr)

	n := fs.get("/srv/deploy.sh")
	if n == nil {
		t.Fatalf("the file never landed (paths: %v)", fs.paths())
	}
	if string(n.data) != "#!/bin/sh\necho hi\n" {
		t.Errorf("content = %q, want the uploaded script", n.data)
	}
	if got := n.mode.Perm(); got != 0o755 {
		t.Errorf("perm = %o, want 755", got)
	}
	for _, p := range fs.paths() {
		if strings.Contains(p, ".webkvm-backup-") {
			t.Errorf("the backup sidecar survived a successful upload: %s (all: %v)", p, fs.paths())
		}
	}
}

func TestUploadWithoutAModeLeavesTheServerDefault(t *testing.T) {
	m, fs, alpha, local := uploadFixture(t)

	job, err := m.StartUpload(alpha, "/srv/plain.txt", local, 0)
	if err != nil {
		t.Fatalf("start upload: %v", err)
	}
	tr := waitJob(t, m, job.ID)
	mustDone(t, tr)

	n := fs.get("/srv/plain.txt")
	if n == nil {
		t.Fatal("the file never landed")
	}
	// memFS models what sftp-server's create gives: 0644.
	if got := n.mode.Perm(); got != 0o644 {
		t.Errorf("perm = %o, want the create default 644 left alone", got)
	}
}

// TestUploadConsumesTheSpooledSource pins the job-private temp file: a
// browser upload that left its spool under dataDir/ssh-transfers/tmp
// would grow /opt/webkvm one upload at a time, forever.
func TestUploadConsumesTheSpooledSource(t *testing.T) {
	m, _, alpha, local := uploadFixture(t)
	job, err := m.StartUpload(alpha, "/srv/plain.txt", local, 0o600)
	if err != nil {
		t.Fatalf("start upload: %v", err)
	}
	tr := waitJob(t, m, job.ID)
	mustDone(t, tr)
	if _, err := os.Stat(local); err == nil {
		t.Errorf("the spool %s survived a finished upload", local)
	}
}
