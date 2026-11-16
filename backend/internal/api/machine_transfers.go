package api

// Machine transfer plumbing that is not tied to one machine's own
// routes: the ownership guard every job endpoint shares, the batch stat
// the dual-pane copy dialog needs before it asks about conflicts, the
// global queue the transfer sidebar reads, and the tar.gz streamer that
// turns a finished directory download into a single browser file.

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"webkvm/internal/sshmachines"
)

// auditMachine records one machine-scoped mutation. The SFTP write
// endpoints used to leave no trace at all: a console-capable account
// could delete or chmod anything on any registered host and the audit
// log would show nothing.
func (h *Handler) auditMachine(r *http.Request, id, action string, detail map[string]any) {
	if h.audit == nil {
		return
	}
	h.audit.Log(auditFor(r, action, "machine:"+id, detail))
}

// machineJob resolves the {job} segment and proves it belongs to the
// {id} in the path, either as the source or as the destination of a
// copy. Without this check any console user could cancel or delete
// another machine's job (and so delete its download result) by id.
//
// A 404 rather than a 403 is deliberate: the id is a uuid, and telling
// an unrelated caller "this job exists but is not yours" leaks the
// inventory of a different account.
func (h *Handler) machineJob(w http.ResponseWriter, r *http.Request, wantKind string) (*sshmachines.Transfer, bool) {
	id := chi.URLParam(r, "id")
	if gateHostCard(w, r, id) {
		return nil, false
	}
	job := h.transfers.Get(chi.URLParam(r, "job"))
	if job == nil || (job.MachineID != id && job.DstMachineID != id) {
		jsonErr(w, http.StatusNotFound, "transfer not found")
		return nil, false
	}
	if wantKind != "" && job.Kind != wantKind {
		jsonErr(w, http.StatusNotFound, "transfer not found")
		return nil, false
	}
	return job, true
}

// ---- batch stat ----

// sftpStatMaxPaths bounds one batch. A few dozen round trips per dialog
// is already generous; past that a caller is probing the remote sshd.
const sftpStatMaxPaths = 200

// sftpStatter is the only call MachineSFTPStat makes on the pooled
// client. Naming it keeps the endpoint's logic reachable without a live
// sshd; *sftp.Client satisfies it as is.
type sftpStatter interface {
	Lstat(string) (os.FileInfo, error)
}

// MachineSFTPStat answers several paths in one round trip. The copy
// flow needs it before it can ask the user about conflicts in a single
// dialog instead of one prompt per colliding file.
//
//	GET /api/ssh-machines/{id}/sftp/stat?paths=/a&paths=/b
func (h *Handler) MachineSFTPStat(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.machineForRequest(w, r); !ok {
		return
	}
	// Paths are validated before the session is resolved: a malformed
	// request must not pay for an SSH dial, nor become a way to make the
	// host dial an untrusted machine on demand.
	paths, err := sftpStatPaths(r.URL.Query()["paths"])
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id := chi.URLParam(r, "id")
	client, err := h.sftpSessions.Get(id)
	if err != nil {
		h.sftpErr(w, err)
		return
	}
	h.sftpSessions.Touch(id)
	jsonResp(w, http.StatusOK, map[string]any{"stats": sftpStatEntries(client, paths)})
}

// sftpStatPaths reads the paths argument: exactly one path per repeated
// ?paths= parameter.
//
// Comma-splitting was the first attempt and it is wrong: "report,
// 2026.txt" is an ordinary remote filename, and turning it into two
// answers for paths nobody asked about is worse than an error. Trimming is
// left out for the same reason — leading and trailing spaces are legal in
// a path.
func sftpStatPaths(chunks []string) ([]string, error) {
	wanted := make([]string, 0, len(chunks))
	for _, p := range chunks {
		if p == "" {
			continue
		}
		wanted = append(wanted, cleanRemotePath(p))
	}
	if len(wanted) == 0 {
		return nil, errors.New("paths is required")
	}
	if len(wanted) > sftpStatMaxPaths {
		return nil, errors.New("too many paths (" + strconv.Itoa(sftpStatMaxPaths) + " max)")
	}
	return wanted, nil
}

func sftpStatEntries(client sftpStatter, paths []string) []map[string]any {
	stats := make([]map[string]any, 0, len(paths))
	for _, p := range paths {
		entry := map[string]any{"path": p, "exists": false}
		// Lstat, not Stat: the copy dialog asks "is that name taken", and
		// a dangling symlink occupies one just as much as a file does.
		if fi, err := client.Lstat(p); err == nil {
			entry["exists"] = true
			entry["is_dir"] = fi.IsDir()
			entry["size"] = fi.Size()
			entry["mode"] = fi.Mode().String()
			entry["mod_time"] = fi.ModTime().Unix()
			entry["is_symlink"] = fi.Mode()&os.ModeSymlink != 0
		}
		stats = append(stats, entry)
	}
	return stats
}

// ---- global queue ----

// hostJobGate applies the host card's admin-only rule to the routes
// that carry a bare job id. The console capability proves "may operate
// on machines", not "may operate on this machine's disk": a job that
// touches the built-in card (as source or as copy destination) is an
// admin surface, and for anyone below admin it must read as absent —
// the same 404 shape as a job id nobody owns. Without this gate the
// global queue leaks the admin's host-card job rows (paths,
// destinations) and the cancel/remove buttons drive a root read/write
// of the host disk for any console account.
func hostJobGate(w http.ResponseWriter, r *http.Request, job *sshmachines.Transfer) bool {
	if job.MachineID != sshmachines.HostMachineID && job.DstMachineID != sshmachines.HostMachineID {
		return false
	}
	if requestIsAdmin(r) {
		return false
	}
	jsonErr(w, http.StatusNotFound, "transfer not found")
	return true
}

// MachineTransfersAll serves the sidebar's single request for every job
// across every machine, instead of one poll per machine.
//
//	GET /api/machine-transfers?machines=a,b&limit=200
func (h *Handler) MachineTransfersAll(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 1000 {
			jsonErr(w, http.StatusBadRequest, "limit must be between 1 and 1000")
			return
		}
		limit = n
	}
	jobs := h.transfers.ListAll(limit)

	// The host card's jobs are admin-only everywhere else (gateHostCard);
	// the queue row would be the one surface where they leak, so they are
	// dropped before any display filter — for a non-admin the response is
	// the same shape as the card itself: absent.
	if !requestIsAdmin(r) {
		kept := make([]map[string]any, 0, len(jobs))
		for _, j := range jobs {
			src, _ := j["machine_id"].(string)
			dst, _ := j["dst_machine_id"].(string)
			if src == sshmachines.HostMachineID || dst == sshmachines.HostMachineID {
				continue
			}
			kept = append(kept, j)
		}
		jobs = kept
	}

	// machines= is a display filter only. It is applied after ListAll so
	// an unrelated id cannot be probed: the response never says "that
	// machine has jobs but you cannot see them".
	if want := r.URL.Query().Get("machines"); want != "" {
		allowed := map[string]bool{}
		for _, id := range strings.Split(want, ",") {
			if id = strings.TrimSpace(id); id != "" {
				allowed[id] = true
			}
		}
		filtered := make([]map[string]any, 0, len(jobs))
		for _, j := range jobs {
			src, _ := j["machine_id"].(string)
			dst, _ := j["dst_machine_id"].(string)
			if allowed[src] || allowed[dst] {
				filtered = append(filtered, j)
			}
		}
		jobs = filtered
	}
	jsonResp(w, http.StatusOK, jobs)
}

// MachineTransfersClearFinished is the queue's "clear completed": every
// terminal job unloads in one server pass instead of one DELETE per row,
// and running or paused jobs are untouched (RemoveFinished skips them),
// so a clear can never stop a live transfer.
func (h *Handler) MachineTransfersClearFinished(w http.ResponseWriter, r *http.Request) {
	removed, machines := h.transfers.RemoveFinished()
	for _, id := range machines {
		h.auditMachine(r, id, "ssh_machine.transfer.clear_finished", map[string]any{"removed": removed})
	}
	jsonResp(w, http.StatusOK, map[string]any{"removed": removed})
}

// MachineTransferCancelJob and MachineTransferRemoveJob are the
// job-granular twins of the per-machine routes, for a queue row that
// only knows the job id. The console capability gate on the route group
// authorizes ordinary jobs (a console account can already drive every
// machine's terminal and file browser); a job that touches the built-in
// host card is an admin surface on its own (hostJobGate).
func (h *Handler) MachineTransferCancelJob(w http.ResponseWriter, r *http.Request) {
	job := h.transfers.Get(chi.URLParam(r, "job"))
	if job == nil {
		jsonErr(w, http.StatusNotFound, "transfer not found")
		return
	}
	if hostJobGate(w, r, job) {
		return
	}
	if err := h.transfers.Cancel(job.ID); err != nil {
		jsonErr(w, http.StatusConflict, err.Error())
		return
	}
	h.auditMachine(r, job.MachineID, "ssh_machine.transfer.cancel", map[string]any{"job": job.ID, "kind": job.Kind})
	jsonResp(w, http.StatusOK, map[string]any{"canceled": job.ID})
}

func (h *Handler) MachineTransferRemoveJob(w http.ResponseWriter, r *http.Request) {
	job := h.transfers.Get(chi.URLParam(r, "job"))
	if job == nil {
		jsonErr(w, http.StatusNotFound, "transfer not found")
		return
	}
	if hostJobGate(w, r, job) {
		return
	}
	if err := h.transfers.Remove(job.ID); err != nil {
		jsonErr(w, http.StatusConflict, err.Error())
		return
	}
	h.auditMachine(r, job.MachineID, "ssh_machine.transfer.remove", map[string]any{"job": job.ID, "kind": job.Kind})
	w.WriteHeader(http.StatusNoContent)
}

// MachineTransferPauseJob and MachineTransferResumeJob park and release
// a job. The pause button lives on the global queue rows — the
// per-machine surface has no pause of its own — so these are the only
// entry points, and they share the same authorization as cancel/remove.
func (h *Handler) MachineTransferPauseJob(w http.ResponseWriter, r *http.Request) {
	job := h.transfers.Get(chi.URLParam(r, "job"))
	if job == nil {
		jsonErr(w, http.StatusNotFound, "transfer not found")
		return
	}
	if hostJobGate(w, r, job) {
		return
	}
	if err := h.transfers.Pause(job.ID); err != nil {
		jsonErr(w, http.StatusConflict, err.Error())
		return
	}
	h.auditMachine(r, job.MachineID, "ssh_machine.transfer.pause", map[string]any{"job": job.ID, "kind": job.Kind})
	jsonResp(w, http.StatusOK, map[string]any{"paused": job.ID})
}

func (h *Handler) MachineTransferResumeJob(w http.ResponseWriter, r *http.Request) {
	job := h.transfers.Get(chi.URLParam(r, "job"))
	if job == nil {
		jsonErr(w, http.StatusNotFound, "transfer not found")
		return
	}
	if hostJobGate(w, r, job) {
		return
	}
	if err := h.transfers.Resume(job.ID); err != nil {
		jsonErr(w, http.StatusConflict, err.Error())
		return
	}
	h.auditMachine(r, job.MachineID, "ssh_machine.transfer.resume", map[string]any{"job": job.ID, "kind": job.Kind})
	jsonResp(w, http.StatusOK, map[string]any{"resumed": job.ID})
}

// ---- directory results as one tar.gz ----

// streamTransferDir packs the job's local tree into a single
// archive/tar+gzip stream. Nothing is pre-generated on disk: the browser
// pulls one download and /opt/webkvm does not gain a second copy of
// whatever the user just fetched.
func (h *Handler) streamTransferDir(w http.ResponseWriter, job *sshmachines.Transfer, name string) {
	base := job.LocalFile
	if fi, err := os.Stat(base); err != nil || !fi.IsDir() {
		jsonErr(w, http.StatusNotFound, "transfer directory is missing")
		return
	}
	root := sanitizeArchiveRoot(name)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+root+`.tar.gz"`)
	// No Content-Length: the archive is produced while it streams.

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	walkErr := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// One unreadable entry must not abort the whole archive.
			return nil //nolint:nilerr // deliberate skip
		}
		rel, rerr := filepath.Rel(base, p)
		if rerr != nil || rel == "." {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		return writeTarEntry(tw, p, path.Join(root, filepath.ToSlash(rel)), info)
	})
	if cerr := tw.Close(); walkErr == nil {
		walkErr = cerr
	}
	if cerr := gz.Close(); walkErr == nil {
		walkErr = cerr
	}
	if walkErr != nil {
		// Headers are already on the wire, so the only honest report left
		// is this line: the browser sees a truncated archive and has no way
		// to tell. That makes it an operator-visible failure, not a warning.
		slog.Error("ssh_machine_transfer_archive_failed", "job", job.ID, "path", base, "err", walkErr.Error())
	}
}

// writeTarEntry emits one tree member. Symlinks are stored as links
// rather than dereferenced, so an archive of a remote tree cannot smuggle
// in the contents of files the user never selected.
//
// Every failure is returned rather than skipped: a member is declared by
// its header's Size, so a header without its payload desynchronises the
// stream and every record after it reads as garbage — while the gzip
// still terminates cleanly and the browser reports a successful download
// of a corrupt archive.
func writeTarEntry(tw *tar.Writer, diskPath, name string, info os.FileInfo) error {
	name = path.Clean(strings.TrimPrefix(name, "/"))
	if name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return nil
	}
	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(diskPath)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", diskPath, err)
		}
		return tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeSymlink,
			Name:     name,
			Linkname: target,
			Mode:     0o777,
			ModTime:  info.ModTime(),
		})
	case info.IsDir():
		return tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir,
			Name:     name + "/",
			Mode:     int64(mode.Perm()),
			ModTime:  info.ModTime(),
		})
	case !mode.IsRegular():
		return nil // sockets, devices and fifos have no archive form
	}
	// Opened BEFORE the header: see the note above.
	f, err := os.Open(diskPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", diskPath, err)
	}
	defer f.Close()
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     info.Size(),
		Mode:     int64(mode.Perm()),
		ModTime:  info.ModTime(),
	}); err != nil {
		return err
	}
	n, err := io.Copy(tw, f)
	if err != nil {
		return fmt.Errorf("read %s: %w", diskPath, err)
	}
	if n != info.Size() {
		return fmt.Errorf("short read %s: archived %d of %d bytes", diskPath, n, info.Size())
	}
	return nil
}

// sanitizeArchiveRoot keeps a remote path's basename out of the header
// and out of the tar member prefixes.
func sanitizeArchiveRoot(name string) string {
	name = path.Base(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ' ':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), ". ")
	if out == "" || out == ".." {
		return "download"
	}
	return out
}

// decodeJSONLimited guards the JSON branches of routes that are exempt
// from the global body cap because their multipart branch installs a
// much larger one of its own.
func decodeJSONLimited(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return tooBig
		}
		return err
	}
	return nil
}
