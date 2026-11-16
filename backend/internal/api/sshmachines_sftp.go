package api

// SFTP file browser + transfer endpoints for SSH machines and the
// built-in host card.
//
// Endpoints (all under /api/ssh-machines/{id}, console-capability gate):
//
//	GET    /sftp/list?path=          directory listing (empty path = home)
//	GET    /sftp/read?path&offset   partial file read (base64, preview cap)
//	POST   /sftp/write              create/overwrite a small file
//	POST   /sftp/mkdir
//	POST   /sftp/delete
//	POST   /sftp/rename
//	POST   /sftp/chmod
//	GET    /sftp/realpath?path=
//	GET    /sftp/sizes?paths=       summed size of each listed directory
//	POST   /transfers               start a transfer (multipart upload /
//	                                 JSON download / JSON download-batch)
//	GET    /transfers               this machine's jobs
//	POST   /transfers/{job}/cancel
//	GET    /transfers/{job}/result  stream the finished download
//	DELETE /transfers/{job}
//
// The pooled per-machine SFTP session (SFTPSessionRegistry) is shared
// across all of these; idle GC is in the registry. The host card needs
// no dial and no host key: its session is the in-process pair, and only
// an admin may ask for it (gateHostCard).

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/pkg/sftp"

	"webkvm/internal/sshmachines"
)

const (
	// sftpPreviewCap bounds a single /sftp/read response (256 KiB).
	// Larger files travel through the download transfer job.
	sftpPreviewCap = 256 * 1024
	// sftpUploadInlineCap bounds what /sftp/write accepts (768 KiB of
	// file). Anything bigger belongs in an upload transfer (streamed, with
	// progress). The number is the global JSON body cap from
	// middleware_bodylimit.go divided by base64's 4/3 expansion, rounded
	// down: over that, MaxBytesReader cuts the request with a 400 before
	// this handler can answer with the message that tells the user what to
	// do instead, so a higher cap here would be unreachable.
	sftpUploadInlineCap = 768 * 1024
)

// machineOrHostCard answers a machine-scoped route with the record behind
// the id: a stored machine, or the built-in host card for an admin.
//
// It applies no protocol rule on purpose. The transfer endpoint serves
// actions that need opposite transports — SFTP wants ssh, ZMODEM staging
// wants telnet or serial — so each branch asserts the one it actually
// requires, and a rule here would silently close off the other.
func (h *Handler) machineOrHostCard(w http.ResponseWriter, r *http.Request, id string) (*sshmachines.Machine, bool) {
	if gateHostCard(w, r, id) {
		return nil, false
	}
	if id == sshmachines.HostMachineID {
		return sshmachines.SyntheticHost(), true
	}
	machine, ok := h.machines.Get(id)
	if !ok {
		jsonErr(w, http.StatusNotFound, "machine not found")
		return nil, false
	}
	return machine, true
}

// resolveFileMachine is machineOrHostCard plus the file plane's protocol
// rule: a machine you cannot browse cannot be a transfer destination
// either.
func (h *Handler) resolveFileMachine(w http.ResponseWriter, r *http.Request, id string) (*sshmachines.Machine, bool) {
	machine, ok := h.machineOrHostCard(w, r, id)
	if !ok {
		return nil, false
	}
	if !requireFileCarrier(w, machine) {
		return nil, false
	}
	return machine, true
}

// requireFileCarrier answers 400 for a machine with no filesystem behind
// it. The host card carries files by definition (see CarriesFiles).
func requireFileCarrier(w http.ResponseWriter, machine *sshmachines.Machine) bool {
	if machine.CarriesFiles() {
		return true
	}
	jsonErr(w, http.StatusBadRequest, "files need an ssh machine (this machine is "+machine.Protocol+")")
	return false
}

// machineForRequest is the pre-dial half of sftpClientFor: everything
// that can be answered from the registry alone. Endpoints that want to
// validate their input before paying for an SSH session use this one
// directly.
func (h *Handler) machineForRequest(w http.ResponseWriter, r *http.Request) (*sshmachines.Machine, bool) {
	return h.resolveFileMachine(w, r, chi.URLParam(r, "id"))
}

// sftpClientFor resolves the pooled SFTP client, translating the two
// trust failures into 409s the UI can turn into the re-pin dialog.
func (h *Handler) sftpClientFor(w http.ResponseWriter, r *http.Request) (*sftp.Client, *sshmachines.Machine, bool) {
	machine, ok := h.machineForRequest(w, r)
	if !ok {
		return nil, nil, false
	}
	id := machine.ID
	client, err := h.sftpSessions.Get(id)
	if err != nil {
		h.sftpErr(w, err)
		return nil, nil, false
	}
	h.sftpSessions.Touch(id)
	return client, machine, true
}

// sftpErr maps the typed dial failures onto HTTP codes.
func (h *Handler) sftpErr(w http.ResponseWriter, err error) {
	var hke *sshmachines.HostKeyError
	if errors.As(err, &hke) {
		switch {
		case errors.Is(hke.Err, sshmachines.ErrHostKeyNotPinned):
			jsonResp(w, http.StatusConflict, map[string]any{
				"error":       "hostkey_untrusted",
				"status":      "hostkey_untrusted",
				"fingerprint": hke.PresentedFull,
			})
			return
		case errors.Is(hke.Err, sshmachines.ErrHostKeyChanged):
			jsonResp(w, http.StatusConflict, map[string]any{
				"error":       "hostkey_changed",
				"status":      "hostkey_changed",
				"fingerprint": hke.PresentedFull,
				"pinned":      hke.Expected,
			})
			return
		}
	}
	var subsys *sshmachines.SFTPSubsystemError
	if errors.As(err, &subsys) {
		jsonErr(w, http.StatusBadGateway, subsys.Error())
		return
	}
	jsonErr(w, http.StatusBadGateway, err.Error())
}

type sftpEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime int64  `json:"mod_time"`
}

func sftpEntryFrom(info os.FileInfo, dir string) sftpEntry {
	name := info.Name()
	p := name
	if dir != "/" {
		p = dir + "/" + name
	}
	return sftpEntry{
		Name:    name,
		Path:    path.Clean(p),
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		Mode:    info.Mode().String(),
		ModTime: info.ModTime().Unix(),
	}
}

func (h *Handler) MachineSFTPList(w http.ResponseWriter, r *http.Request) {
	client, _, ok := h.sftpClientFor(w, r)
	if !ok {
		return
	}
	dir := r.URL.Query().Get("path")
	if dir == "" {
		// A fresh SFTP session starts in the remote user's home directory
		// (OpenSSH sftp-server default), so the server's working directory
		// IS the home — Getwd() is how the client reads it.
		home, err := client.Getwd()
		if err != nil {
			jsonErr(w, http.StatusBadGateway, "home: "+err.Error())
			return
		}
		dir = home
	} else {
		dir = cleanRemotePath(dir)
	}
	infos, err := client.ReadDir(dir)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	entries := make([]sftpEntry, 0, len(infos))
	for _, info := range infos {
		entries = append(entries, sftpEntryFrom(info, dir))
	}
	// Directories first, then case-insensitive name order.
	sortSFTPEntries(entries)
	jsonResp(w, http.StatusOK, map[string]any{
		"path":    dir,
		"entries": entries,
	})
}

func (h *Handler) MachineSFTPRead(w http.ResponseWriter, r *http.Request) {
	client, _, ok := h.sftpClientFor(w, r)
	if !ok {
		return
	}
	p := cleanRemotePath(r.URL.Query().Get("path"))
	q := r.URL.Query()
	offset, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
	limit, _ := strconv.ParseInt(q.Get("limit"), 10, 64)
	if limit <= 0 || limit > sftpPreviewCap {
		limit = sftpPreviewCap
	}
	f, err := client.Open(p)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if fi.IsDir() {
		jsonErr(w, http.StatusBadRequest, "path is a directory")
		return
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	buf := make([]byte, limit)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	truncated := offset+int64(n) < fi.Size()
	jsonResp(w, http.StatusOK, map[string]any{
		"path":      p,
		"size":      fi.Size(),
		"mod_time":  fi.ModTime().Unix(),
		"offset":    offset,
		"data":      base64.StdEncoding.EncodeToString(buf[:n]),
		"truncated": truncated,
	})
}

type sftpPathBody struct {
	Path string `json:"path"`
}

type sftpWriteBody struct {
	Path string `json:"path"`
	Data string `json:"data"` // base64
	Mode string `json:"mode,omitempty"`
}

func (h *Handler) MachineSFTPWrite(w http.ResponseWriter, r *http.Request) {
	client, _, ok := h.sftpClientFor(w, r)
	if !ok {
		return
	}
	var body sftpWriteBody
	if err := decodeBody(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	p := cleanRemotePath(body.Path)
	data, err := base64.StdEncoding.DecodeString(body.Data)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "data must be base64")
		return
	}
	if len(data) > sftpUploadInlineCap {
		jsonErr(w, http.StatusRequestEntityTooLarge, "file exceeds the inline write limit; use an upload transfer")
		return
	}
	// Published through the same scratch-then-rename transaction the
	// upload jobs use: Create+Write truncated the destination first and
	// left it half-full whenever the connection broke mid-write.
	if err := sshmachines.PutInline(client, p, data, 0); err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if body.Mode != "" {
		if err := applySFTPMode(client, p, body.Mode); err != nil {
			jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	h.auditMachine(r, chi.URLParam(r, "id"), "ssh_machine.sftp.write", map[string]any{"path": p, "size": len(data)})
	jsonResp(w, http.StatusOK, map[string]any{"path": p, "size": len(data)})
}

func (h *Handler) MachineSFTPMkdir(w http.ResponseWriter, r *http.Request) {
	client, _, ok := h.sftpClientFor(w, r)
	if !ok {
		return
	}
	var body sftpPathBody
	if err := decodeBody(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	p := cleanRemotePath(body.Path)
	if err := client.Mkdir(p); err != nil {
		// Asking for a directory that already exists is not a failure. The
		// browser creates the missing ancestors of a dropped path one level
		// at a time, and any of them may already be there; failing the
		// first one that is would abort the whole batch over a no-op.
		if info, serr := client.Stat(p); serr == nil && info.IsDir() {
			jsonResp(w, http.StatusOK, map[string]any{"path": p, "existed": true})
			return
		}
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	h.auditMachine(r, chi.URLParam(r, "id"), "ssh_machine.sftp.mkdir", map[string]any{"path": p})
	jsonResp(w, http.StatusOK, map[string]any{"path": p})
}

func (h *Handler) MachineSFTPDelete(w http.ResponseWriter, r *http.Request) {
	client, _, ok := h.sftpClientFor(w, r)
	if !ok {
		return
	}
	var body sftpPathBody
	if err := decodeBody(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	p := cleanRemotePath(body.Path)
	if info, err := client.Stat(p); err == nil && info.IsDir() {
		// Refuse non-empty directories; an empty one goes via RemoveDir.
		if entries, err := client.ReadDir(p); err == nil && len(entries) > 0 {
			jsonErr(w, http.StatusConflict, "directory is not empty")
			return
		}
		if err := client.RemoveDirectory(p); err != nil {
			jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
	} else {
		if err := client.Remove(p); err != nil {
			jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	h.auditMachine(r, chi.URLParam(r, "id"), "ssh_machine.sftp.delete", map[string]any{"path": p})
	jsonResp(w, http.StatusOK, map[string]any{"deleted": p})
}

type sftpRenameBody struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (h *Handler) MachineSFTRename(w http.ResponseWriter, r *http.Request) {
	client, _, ok := h.sftpClientFor(w, r)
	if !ok {
		return
	}
	var body sftpRenameBody
	if err := decodeBody(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	from := cleanRemotePath(body.From)
	to := cleanRemotePath(body.To)
	if err := client.Rename(from, to); err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	h.auditMachine(r, chi.URLParam(r, "id"), "ssh_machine.sftp.rename", map[string]any{"from": from, "to": to})
	jsonResp(w, http.StatusOK, map[string]any{"from": from, "to": to})
}

type sftpChmodBody struct {
	Path string `json:"path"`
	Mode string `json:"mode"` // "755"
}

func (h *Handler) MachineSFTPChmod(w http.ResponseWriter, r *http.Request) {
	client, _, ok := h.sftpClientFor(w, r)
	if !ok {
		return
	}
	var body sftpChmodBody
	if err := decodeBody(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	p := cleanRemotePath(body.Path)
	if err := applySFTPMode(client, p, body.Mode); err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	h.auditMachine(r, chi.URLParam(r, "id"), "ssh_machine.sftp.chmod", map[string]any{"path": p, "mode": body.Mode})
	jsonResp(w, http.StatusOK, map[string]any{"path": p, "mode": body.Mode})
}

func applySFTPMode(client *sftp.Client, p, mode string) error {
	n, err := parseSFTPMode(mode)
	if err != nil {
		return err
	}
	return client.Chmod(p, n)
}

// parseSFTPMode reads a 3- or 4-digit octal permission string. The SFTP
// subsystem takes no perm on create, so every caller that wants one has
// to follow the write with a chmod — here and in the upload job.
//
// Special bits are dropped on purpose: a 4-digit "4755" would hand the
// remote a setuid-root binary owned by the uploading user whenever
// sftp-server runs as root, and nothing in this UI has a use for that.
func parseSFTPMode(mode string) (os.FileMode, error) {
	if len(mode) < 3 || len(mode) > 4 {
		return 0, errors.New("mode must be 3 or 4 octal digits (e.g. 755)")
	}
	n, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0, errors.New("mode must be octal (e.g. 755)")
	}
	return os.FileMode(n).Perm(), nil
}

func (h *Handler) MachineSFTPRealpath(w http.ResponseWriter, r *http.Request) {
	client, _, ok := h.sftpClientFor(w, r)
	if !ok {
		return
	}
	p := cleanRemotePath(r.URL.Query().Get("path"))
	real, err := client.RealPath(p)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	jsonResp(w, http.StatusOK, map[string]any{"realpath": real})
}

// sftpSizesMaxPaths bounds one batch. The UI only asks about the
// directories visible in one pane, so the cap sits far above anything a
// single listing can show.
const sftpSizesMaxPaths = 128

// sizesDeadline is the whole request window for every walk: it bounds
// both how many listings fly and how slow the link may be. A folder
// size is cosmetic, and ten seconds is far more than anyone waits for a
// number next to a row.
const sizesDeadline = 10 * time.Second

// downloadBatchMaxPaths bounds the download-batch action. It sits well
// above a pane's visible row count; the planner and the archive both
// cope with more, but a thousand roots in one tar.gz is no longer "what
// I selected" and the JSON body cap would start to matter.
const downloadBatchMaxPaths = 64

// MachineSFPTSizes answers the file browser's second request for a
// number a listing cannot carry: a directory's FileInfo is a block size,
// not the sum of what is under it.
//
//	GET /api/ssh-machines/{id}/sftp/sizes?paths=/a&paths=/b
//
// One entry per path: {path, size, truncated, error?}. truncated is
// true when the walk's caps (depth, entry count, deadline) cut the sum
// short and the UI renders that number with an approximation mark. A
// per-path error never fails the request: one unreadable directory
// among ten must not blank the other nine.
func (h *Handler) MachineSFPTSizes(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.machineForRequest(w, r); !ok {
		return
	}
	paths, err := sftpSizesPaths(r.URL.Query()["paths"])
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

	ctx, cancel := context.WithTimeout(r.Context(), sizesDeadline)
	defer cancel()
	jsonResp(w, http.StatusOK, map[string]any{"sizes": folderSizeEntries(ctx, client, paths)})
}

// sftpSizesPaths reads the paths argument: one path per repeated
// ?paths= parameter, no comma splitting (a comma is a legal character
// in a filename), no trimming — the same rules as sftpStatPaths.
func sftpSizesPaths(chunks []string) ([]string, error) {
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
	if len(wanted) > sftpSizesMaxPaths {
		return nil, errors.New("too many paths (" + strconv.Itoa(sftpSizesMaxPaths) + " max)")
	}
	return wanted, nil
}

// folderSizeEntries walks each path. The deadline is per request, not
// per path: an entry that starts late gets what the window still has,
// and a walk the deadline catches mid-level returns its partial sum with
// truncated set.
func folderSizeEntries(ctx context.Context, fs sshmachines.SizeFS, paths []string) []map[string]any {
	sizes := make([]map[string]any, 0, len(paths))
	for _, p := range paths {
		entry := map[string]any{"path": p}
		if ctx.Err() != nil {
			// The window is gone before this walk could start: a
			// partial sum of zero would read as "empty folder".
			entry["error"] = "size walk timed out"
		} else if size, truncated, err := sshmachines.FolderSize(ctx, fs, p, sshmachines.DefaultSizeLimits); err != nil {
			entry["error"] = err.Error()
		} else {
			entry["size"] = size
			entry["truncated"] = truncated
		}
		sizes = append(sizes, entry)
	}
	return sizes
}

// ---- transfers ----

// MachineTransferStart handles both directions:
//
//	multipart/form-data with fields action=upload, remote_path, file=@f
//	-> spools the upload to a temp file, then pushes it (D4-lite).
//
//	JSON {"action":"download","remote_path":"/...","file_name":"..."}
//	-> pulls the file to the on-host staging area; the SPA fetches it
//	from /transfers/{job}/result.
//
//	JSON {"action":"download-batch","paths":["/..."],"file_name":"..."}
//	-> several roots pulled into one tree and delivered as a single
//	tar.gz through the same /result endpoint.
func (h *Handler) MachineTransferStart(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	machine, ok := h.machineOrHostCard(w, r, id)
	if !ok {
		return
	}

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		// The global 1 MiB body cap is exempted for this route (see
		// ownLimitRoutes), so the handler owns the limit: 4 GiB, in
		// line with the D4-lite spool posture. Larger transfers should
		// chunk client-side in a later phase.
		const maxTransferBody int64 = 4 << 30
		r.Body = http.MaxBytesReader(w, r.Body, maxTransferBody)
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				jsonErr(w, http.StatusRequestEntityTooLarge, "upload exceeds the 4 GiB limit")
				return
			}
			jsonErr(w, http.StatusBadRequest, "parse multipart: "+err.Error())
			return
		}
		action := r.FormValue("action")
		if action == "zmodem-stage" {
			h.stageZmodemUpload(w, r, machine)
			return
		}
		remotePath := r.FormValue("remote_path")
		file, header, err := r.FormFile("file")
		if err != nil {
			jsonErr(w, http.StatusBadRequest, "missing file part")
			return
		}
		defer file.Close()
		if action != "upload" {
			jsonErr(w, http.StatusBadRequest, "multipart transfers only support action=upload")
			return
		}
		if !requireFileCarrier(w, machine) {
			return
		}
		// Optional octal mode (0755 for a script the user just pushed).
		// Validated before the spool so a rejected request never leaves a
		// temp file behind.
		var uploadMode os.FileMode
		if mv := r.FormValue("mode"); mv != "" {
			uploadMode, err = parseSFTPMode(mv)
			if err != nil {
				jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		spooled, err := spoolUploadedFile(file, h.cfg.DataDir)
		if err != nil {
			jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
		if remotePath == "" {
			remotePath = "/" + header.Filename
		}
		job, err := h.transfers.StartUpload(id, remotePath, spooled, uploadMode)
		if err != nil {
			_ = os.Remove(spooled)
			jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
		detail := map[string]any{"kind": "upload", "remote_path": remotePath}
		if uploadMode != 0 {
			detail["mode"] = strconv.FormatUint(uint64(uploadMode), 8)
		}
		h.auditMachine(r, id, "ssh_machine.transfer.start", detail)
		jsonResp(w, http.StatusAccepted, job.Snapshot())
		return
	}

	var body struct {
		Action       string   `json:"action"`
		RemotePath   string   `json:"remote_path"`
		Paths        []string `json:"paths"`
		FileName     string   `json:"file_name"`
		DstMachineID string   `json:"dst_machine_id"`
		DstDir       string   `json:"dst_dir"`
		Recursive    *bool    `json:"recursive"`
		Overwrite    string   `json:"overwrite"`
	}
	// This route is exempt from the global 1 MiB cap because its
	// multipart branch installs a 4 GiB one; the JSON branch had no
	// limit at all until this line.
	if err := decodeJSONLimited(w, r, &body, 1<<20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			jsonErr(w, http.StatusRequestEntityTooLarge, "request body exceeds 1 MiB")
			return
		}
		jsonErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	switch body.Action {
	case "download":
		// An empty path used to decode to "/" (CleanRemotePath), which
		// would have planned a download of the remote root. Name the
		// field explicitly so a client typo reads as a bad request.
		if strings.TrimSpace(body.RemotePath) == "" {
			jsonErr(w, http.StatusBadRequest, "remote_path is required")
			return
		}
		if !requireFileCarrier(w, machine) {
			return
		}
		job, err := h.transfers.StartDownload(id, body.RemotePath, body.FileName)
		if err != nil {
			jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
		h.auditMachine(r, id, "ssh_machine.transfer.start", map[string]any{
			"kind": "download", "remote_path": body.RemotePath,
		})
		jsonResp(w, http.StatusAccepted, job.Snapshot())
	case "download-batch":
		// Several selected roots as one job: the planner dedupes the
		// top-level names, and /result packs the merged tree into a
		// single tar.gz. A one-entry selection still goes through the
		// plain download branch, which the SPA prefers.
		if !requireFileCarrier(w, machine) {
			return
		}
		if len(body.Paths) == 0 || len(body.Paths) > downloadBatchMaxPaths {
			jsonErr(w, http.StatusBadRequest,
				fmt.Sprintf("paths must hold between 1 and %d entries", downloadBatchMaxPaths))
			return
		}
		job, err := h.transfers.StartDownloadBatch(id, body.Paths, body.FileName)
		if err != nil {
			jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
		h.auditMachine(r, id, "ssh_machine.transfer.start", map[string]any{
			"kind": "download-batch", "paths": len(body.Paths),
		})
		jsonResp(w, http.StatusAccepted, job.Snapshot())
	case "copy":
		// Machine to machine: {id} is the source. Both ends need a
		// filesystem, and the destination is resolved here because its
		// absence must read as a 404.
		if strings.TrimSpace(body.RemotePath) == "" {
			jsonErr(w, http.StatusBadRequest, "remote_path is required")
			return
		}
		if body.DstMachineID == "" {
			jsonErr(w, http.StatusBadRequest, "dst_machine_id is required")
			return
		}
		if _, ok := h.resolveFileMachine(w, r, body.DstMachineID); !ok {
			return
		}
		if !requireFileCarrier(w, machine) {
			return
		}
		recursive := true
		if body.Recursive != nil {
			recursive = *body.Recursive
		}
		job, err := h.transfers.StartCopy(id, body.RemotePath, body.DstMachineID, body.DstDir, body.Overwrite, recursive)
		if err != nil {
			jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
		h.auditMachine(r, id, "ssh_machine.transfer.start", map[string]any{
			"kind": "copy", "remote_path": body.RemotePath,
			"dst_machine_id": body.DstMachineID, "dst_dir": body.DstDir,
			"recursive": recursive, "overwrite": body.Overwrite,
		})
		jsonResp(w, http.StatusAccepted, job.Snapshot())
	default:
		jsonErr(w, http.StatusBadRequest, "json transfers support action=download, download-batch or copy")
	}
}

func (h *Handler) MachineTransferList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if gateHostCard(w, r, id) {
		return
	}
	jsonResp(w, http.StatusOK, h.transfers.List(id))
}

func (h *Handler) MachineTransferCancel(w http.ResponseWriter, r *http.Request) {
	job, ok := h.machineJob(w, r, "")
	if !ok {
		return
	}
	if err := h.transfers.Cancel(job.ID); err != nil {
		jsonErr(w, http.StatusConflict, err.Error())
		return
	}
	h.auditMachine(r, chi.URLParam(r, "id"), "ssh_machine.transfer.cancel", map[string]any{"job": job.ID, "kind": job.Kind})
	jsonResp(w, http.StatusOK, map[string]any{"canceled": job.ID})
}

// MachineTransferResult streams the on-host file of a finished download.
// A directory job's LocalFile is a tree, which travels as one tar.gz
// built on the fly.
func (h *Handler) MachineTransferResult(w http.ResponseWriter, r *http.Request) {
	job, ok := h.machineJob(w, r, "download")
	if !ok {
		return
	}
	// The pin is what makes the rest of this handler safe: while it is
	// held, neither the queue's eviction cap nor a second browser's list
	// request may delete the file or tree being streamed.
	if !job.PinFetch() {
		jsonErr(w, http.StatusConflict, "transfer is not finished (status: "+job.Status()+")")
		return
	}
	defer job.UnpinFetch()
	name := job.FileName
	if name == "" {
		name = path.Base(job.RemotePath)
	}
	if job.IsDirJob() {
		h.streamTransferDir(w, job, name)
		return
	}
	// Opened before anything is written: a 404 after a committed 200 +
	// Content-Length is a corrupt download the browser cannot distinguish
	// from a good one.
	f, err := os.Open(job.LocalFile)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "transfer file is missing")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, "")+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

func (h *Handler) MachineTransferRemove(w http.ResponseWriter, r *http.Request) {
	job, ok := h.machineJob(w, r, "")
	if !ok {
		return
	}
	if err := h.transfers.Remove(job.ID); err != nil {
		jsonErr(w, http.StatusConflict, err.Error())
		return
	}
	h.auditMachine(r, chi.URLParam(r, "id"), "ssh_machine.transfer.remove", map[string]any{"job": job.ID, "kind": job.Kind})
	w.WriteHeader(http.StatusNoContent)
}

// spoolUploadedFile copies an in-flight multipart file to a private temp
// file under dataDir/ssh-transfers/tmp so the transfer job owns it
// (and cleans it up) after this request returns.
func spoolUploadedFile(file io.Reader, dataDir string) (string, error) {
	dir := filepath.Join(dataDir, "ssh-transfers", "tmp")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	out, err := os.CreateTemp(dir, "upload-*")
	if err != nil {
		return "", err
	}
	name := out.Name()
	if _, err := io.Copy(out, file); err != nil {
		_ = out.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// small helpers

// cleanRemotePath is the handlers' view of sshmachines.CleanRemotePath:
// the same rule the transfer jobs apply to the same strings, so a path
// normalized here and one normalized there cannot name two files.
func cleanRemotePath(p string) string {
	return sshmachines.CleanRemotePath(p)
}

// sortSFTPEntries: directories first, then case-insensitive name.
func sortSFTPEntries(entries []sftpEntry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0; j-- {
			a, b := entries[j], entries[j-1]
			if a.IsDir != b.IsDir {
				if !a.IsDir {
					break
				}
			} else if strings.ToLower(a.Name) <= strings.ToLower(b.Name) {
				break
			}
			entries[j], entries[j-1] = b, a
		}
	}
}
