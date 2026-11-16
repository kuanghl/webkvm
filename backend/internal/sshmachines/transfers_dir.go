package sshmachines

// Recursive jobs: downloading a remote directory onto the webkvm host
// (for the browser to pull as tar.gz) and copying a file or tree
// between two registered machines.
//
// A machine-to-machine copy streams source → webkvm → destination with
// a 64 KiB buffer and never touches the host disk. Spooling instead
// would double the latency, cap the transferable size by whatever is
// free under /opt/webkvm, and leave gigabytes of debris whenever a job
// is cancelled mid-flight.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// StartCopy queues a machine → machine copy of srcPath into dstDir on
// dstMachineID. recursive expands a directory; a single file is copied
// either way.
func (m *TransferManager) StartCopy(machineID, srcPath, dstMachineID, dstDir, overwrite string, recursive bool) (*Transfer, error) {
	src, ok := m.machineForTransfer(machineID)
	if !ok {
		return nil, errors.New("source machine not found")
	}
	if !src.CarriesFiles() {
		return nil, fmt.Errorf("transfers are only available for ssh machines (this machine is %s)", src.Protocol)
	}
	dst, ok := m.machineForTransfer(dstMachineID)
	if !ok {
		return nil, errors.New("destination machine not found")
	}
	if !dst.CarriesFiles() {
		return nil, fmt.Errorf("destination must be an ssh machine (that one is %s)", dst.Protocol)
	}
	if m.sftp == nil {
		return nil, errors.New("sftp is not available")
	}

	id := uuid.NewString()
	t := &Transfer{
		ID:           id,
		MachineID:    machineID,
		Machine:      src.Name,
		DstMachineID: dstMachineID,
		DstMachine:   dst.Name,
		Kind:         "copy",
		RemotePath:   CleanRemotePath(srcPath),
		DstDir:       CleanRemotePath(dstDir),
		FileName:     path.Base(CleanRemotePath(srcPath)),
		StartedAt:    time.Now().UTC(),
		status:       TransferRunning,
		relay:        true,
	}
	policy := normalizeOverwrite(overwrite)
	t.mu.Lock()
	t.overwrite = policy
	t.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	m.register(t)

	m.run("ssh_machines_transfer_copy", t, func() { m.runCopy(ctx, t) })
	return t, nil
}

func (m *TransferManager) runCopy(ctx context.Context, t *Transfer) {
	fail := func(err error) {
		t.finish(TransferError, err.Error())
		m.broadcast(t)
	}
	src, releaseSrc, err := m.lease(t.MachineID)
	if err != nil {
		fail(err)
		return
	}
	defer releaseSrc()
	dst, releaseDst, err := m.lease(t.DstMachineID)
	if err != nil {
		fail(err)
		return
	}
	defer releaseDst()

	policy := t.currentPolicy()
	tree, err := m.plan(ctx, t, src)
	if err != nil {
		fail(err)
		return
	}
	// The plan is the denominator: publish it before the first byte so
	// the queue row stops reading "0 B / 0 B" the moment the tree is
	// known, not only when the first chunk lands.
	m.broadcast(t)

	// A directory copy lands as dstDir/<srcName>/… — merging a folder's
	// contents straight into dstDir is the kind of surprise that eats a
	// user's home directory. A single file keeps dstDir as its target.
	dstRoot := t.DstDir
	if t.SrcDir != "" {
		dstRoot = path.Join(t.DstDir, t.SrcDir)
	}
	if err := dst.MkdirAll(dstRoot); err != nil {
		fail(fmt.Errorf("mkdir %s: %w", dstRoot, err))
		return
	}
	for _, d := range tree.Dirs {
		if err := dst.MkdirAll(path.Join(dstRoot, d)); err != nil {
			fail(fmt.Errorf("mkdir %s: %w", d, err))
			return
		}
	}
	// The relay moves in 64 KiB chunks, and the loop below is the only
	// place that can report the walk between start and done: a frame
	// no faster than 300 ms is enough to read and keeps the hub's
	// slow-client drop budget intact.
	var lastBroadcast time.Time
	tick := func() {
		if time.Since(lastBroadcast) > 300*time.Millisecond {
			lastBroadcast = time.Now()
			m.broadcast(t)
		}
	}
	onBytes := func(n int64) {
		t.noteProgress(n)
		tick()
	}
	for _, f := range tree.Files {
		if err := ctx.Err(); err != nil {
			m.markCanceled(t)
			return
		}
		if t.cancelRequested() {
			m.markCanceled(t)
			return
		}
		if err := t.waitIfPaused(ctx); err != nil {
			m.markCanceled(t)
			return
		}
		t.setCurrent(f.RelPath)
		final, act := resolveConflict(dst, path.Join(dstRoot, f.RelPath), policy)
		if act == conflictSkip {
			t.fileDone(1)
			tick()
			continue
		}
		if err := copyOne(ctx, src, dst, f, final, t, onBytes); err != nil {
			// A cancel lands here as an io/context error, not as a
			// clean loop exit: report it as canceled, otherwise the
			// queue shows a red "error" for what the user stopped.
			if ctx.Err() != nil || t.cancelRequested() {
				m.markCanceled(t)
				return
			}
			fail(fmt.Errorf("copy %s: %w", f.RelPath, err))
			return
		}
		t.fileDone(0)
		tick()
	}
	t.finish(TransferDone, "")
	m.broadcast(t)
}

// copyOne moves exactly one walked entry between two machines.
func copyOne(ctx context.Context, src, dst sftpFS, f FileSpec, final string, t *Transfer, onBytes func(int64)) error {
	if f.Kind == specSymlink {
		target, err := src.ReadLink(f.AbsPath)
		if err != nil {
			return err
		}
		// Replace semantics: a link sitting at the destination has to go
		// before the new one can take its place.
		_ = dst.Remove(final)
		return dst.Symlink(target, final)
	}
	in, err := src.Open(f.AbsPath)
	if err != nil {
		return err
	}
	defer in.Close()
	return putStream(ctx, dst, final, in, putOpts{
		JobID:   t.ID,
		Mode:    f.Mode.Perm(),
		ModTime: f.ModTime,
		OnBytes: onBytes,
		Gate:    t.waitIfPaused,
	})
}

// runDownloadDir pulls a remote tree into t.LocalFile, which becomes a
// directory under outDir. MachineTransferResult streams it back as a
// flat tar.gz so the browser needs one request and gets one zip-like
// download instead of N.
func (m *TransferManager) runDownloadDir(ctx context.Context, t *Transfer, client sftpFS) {
	fail := func(err error) {
		t.finish(TransferError, err.Error())
		m.broadcast(t)
	}
	tree, err := m.plan(ctx, t, client)
	if err != nil {
		fail(err)
		return
	}
	base := t.LocalFile
	if err := os.MkdirAll(base, 0o750); err != nil {
		fail(err)
		return
	}
	// The plan published the denominator; tell the queue about it
	// before the first byte so the row is not stuck on "0 B / 0 B".
	m.broadcast(t)
	for _, d := range tree.Dirs {
		p, err := joinUnder(base, d)
		if err != nil {
			fail(err)
			return
		}
		if err := os.MkdirAll(p, 0o750); err != nil {
			fail(fmt.Errorf("mkdir %s: %w", d, err))
			return
		}
	}
	var lastBroadcast time.Time
	tick := func() {
		if time.Since(lastBroadcast) > 300*time.Millisecond {
			lastBroadcast = time.Now()
			m.broadcast(t)
		}
	}
	onBytes := func(n int64) {
		t.noteProgress(n)
		tick()
	}
	for _, f := range tree.Files {
		if err := ctx.Err(); err != nil || t.cancelRequested() {
			m.markCanceled(t)
			return
		}
		if err := t.waitIfPaused(ctx); err != nil {
			m.markCanceled(t)
			return
		}
		t.setCurrent(f.RelPath)
		if err := saveLocal(ctx, client, base, f, t, onBytes); err != nil {
			if ctx.Err() != nil || t.cancelRequested() {
				m.markCanceled(t)
				return
			}
			fail(err)
			return
		}
		t.fileDone(0)
		tick()
	}
	t.mu.Lock()
	t.kindDir = true
	t.mu.Unlock()
	t.finish(TransferDone, "")
	m.broadcast(t)
}

// planFor expands the job's source once, publishing the file count and
// the byte total before any payload moves so the UI's denominator is
// right from the first progress tick.
func (m *TransferManager) planFor(ctx context.Context, t *Transfer, fs sftpFS) (*WalkResult, error) {
	fi, err := fs.Stat(t.RemotePath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", t.RemotePath, err)
	}
	lim := DefaultWalkLimits
	if !fi.IsDir() {
		// A file "tree" is one entry; keep the caller loop uniform.
		// Publish the denominator like the directory branch does: a
		// copy that never set Total would read "0 B / 0 B" in the
		// queue and render no progress bar at all.
		base := path.Base(t.RemotePath)
		t.mu.Lock()
		t.FilesTotal = 1
		t.Total = fi.Size()
		t.mu.Unlock()
		return &WalkResult{
			Files:      []FileSpec{{RelPath: base, AbsPath: t.RemotePath, Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime()}},
			TotalBytes: fi.Size(),
		}, nil
	}
	tree, err := expandTree(ctx, fs, t.RemotePath, lim)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.FilesTotal = len(tree.Files)
	t.Total = tree.TotalBytes
	t.SrcDir = path.Base(t.RemotePath)
	// Whatever the walk refused to descend into has to reach the queue.
	// A job that moved 300 of 20 000 entries because of the depth cap did
	// not fail, and reporting a plain success would leave the user
	// trusting a folder that is missing most of its files.
	t.Skipped += len(tree.Skipped)
	if tree.Truncated {
		t.truncated = true
	}
	if t.FileName == "" {
		t.FileName = t.SrcDir
	}
	t.mu.Unlock()
	if len(tree.Skipped) > 0 || tree.Truncated {
		slog.Warn("ssh_machines_walk_partial",
			"machine", t.Machine, "path", t.RemotePath,
			"files", len(tree.Files), "skipped", len(tree.Skipped),
			"truncated", tree.Truncated)
	}
	return tree, nil
}

// plan dispatches on the job's shape: a batch download (RemotePaths set)
// merges several roots into one tree; anything else is the classic
// single-root walk.
func (m *TransferManager) plan(ctx context.Context, t *Transfer, fs sftpFS) (*WalkResult, error) {
	if len(t.RemotePaths) > 0 {
		return m.planForBatch(ctx, t, fs)
	}
	return m.planFor(ctx, t, fs)
}

// batchPlanMaxEntries caps the WHOLE merged tree, not each root: 64
// roots at the single-root 20k walk cap each is 1.28 million entries —
// hours of serial SFTP and a ~200 MB job in memory. Past the cap the
// batch keeps going truncated (the same semantics a single-root walk
// already reports), so a huge selection degrades to a partial download
// instead of a queue row that never leaves 0%.
const batchPlanMaxEntries = 50000

// batchPlanDeadline bounds planning's wall time, not its work (the entry
// cap does that). It is the backstop for a slow-but-alive link: a fully
// wedged socket outlives it, because a blocked ReadDir hangs in the
// kernel and no context check between entries can interrupt it — that
// case belongs to the watchdog. Ten minutes covers any link on which
// interactive SFTP still works at the full cap (an RTT under ~110 ms).
const batchPlanDeadline = 10 * time.Minute

// planForBatch merges several roots into the one local tree that
// runDownloadDir materializes and streamTransferDir archives.
//
// Every root lands under its own top-level entry named after the root's
// basename, deduplicated against the other roots (a second "logs"
// becomes "logs (2)"), so two same-named folders from different paths
// or machines cannot overwrite each other in the archive. Directory
// roots keep their subtree under the entry; file roots land as the
// entry itself — no self-nesting. The two kinds share the name space:
// a directory and a file both called "notes" at the same level would
// otherwise collide in the tar.
//
// The merge carries the batch budget (batchPlanMaxEntries,
// batchPlanDeadline) on top of each root's own walk caps: a single root
// is bounded by DefaultWalkLimits, but the union of 64 of them is not.
func (m *TransferManager) planForBatch(ctx context.Context, t *Transfer, fs sftpFS) (*WalkResult, error) {
	planCtx, cancel := context.WithTimeout(ctx, batchPlanDeadline)
	defer cancel()
	res := &WalkResult{}
	used := map[string]bool{}
	pick := func(base string) string {
		if !used[base] {
			return base
		}
		for i := 2; i <= 1000; i++ {
			cand := fmt.Sprintf("%s (%d)", base, i)
			if !used[cand] {
				return cand
			}
		}
		return base
	}
	budget := batchPlanMaxEntries
	for _, raw := range t.RemotePaths {
		root := CleanRemotePath(raw)
		if root == "/" {
			// The single-root path is guarded the same way at the
			// API layer; this is the second line for a direct caller.
			return nil, fmt.Errorf("refusing to batch the filesystem root")
		}
		if budget <= 0 {
			// The budget is spent: the remaining roots are reported,
			// not walked — walking them would add round trips to a job
			// that is already past the cap.
			res.Skipped = append(res.Skipped, SkipNote{Path: root, Reason: "batch entry cap reached"})
			res.Truncated = true
			continue
		}
		fi, err := fs.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", root, err)
		}
		prefix := pick(path.Base(root))
		used[prefix] = true
		if !fi.IsDir() {
			budget--
			res.Files = append(res.Files, FileSpec{
				RelPath: prefix, AbsPath: root,
				Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime(),
			})
			res.TotalBytes += fi.Size()
			continue
		}
		// Each root walks within its share of the batch budget, so the
		// merged tree can never exceed batchPlanMaxEntries even though
		// one root's own cap is DefaultWalkLimits.
		lim := WalkLimits{MaxDepth: DefaultWalkLimits.MaxDepth, MaxFiles: budget}
		tree, err := expandTree(planCtx, fs, root, lim)
		if err != nil {
			if errors.Is(planCtx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("planning the batch took over %s: the remote link is slow or unresponsive, select fewer entries", batchPlanDeadline)
			}
			return nil, err
		}
		budget -= len(tree.Dirs) + len(tree.Files)
		res.Dirs = append(res.Dirs, prefix)
		for _, d := range tree.Dirs {
			res.Dirs = append(res.Dirs, prefix+"/"+d)
		}
		for _, f := range tree.Files {
			f.RelPath = prefix + "/" + f.RelPath
			res.Files = append(res.Files, f)
		}
		res.TotalBytes += tree.TotalBytes
		for _, s := range tree.Skipped {
			res.Skipped = append(res.Skipped, SkipNote{Path: prefix + "/" + s.Path, Reason: s.Reason})
		}
		if tree.Truncated {
			res.Truncated = true
		}
	}
	// MkdirAll on the transfer side does not depend on order, but the
	// archive's member order does; deterministic is cheaper to explain.
	sort.Strings(res.Dirs)
	sort.Slice(res.Files, func(i, j int) bool { return res.Files[i].RelPath < res.Files[j].RelPath })

	// FileName is the archive's root name (StartDownloadBatch set it);
	// unlike planFor's single-root default it must never be overwritten.
	t.mu.Lock()
	t.FilesTotal = len(res.Files)
	t.Total = res.TotalBytes
	t.Skipped += len(res.Skipped)
	if res.Truncated {
		t.truncated = true
	}
	t.mu.Unlock()
	if len(res.Skipped) > 0 || res.Truncated {
		slog.Warn("ssh_machines_walk_partial",
			"machine", t.Machine, "paths", fmt.Sprint(t.RemotePaths),
			"files", len(res.Files), "skipped", len(res.Skipped),
			"truncated", res.Truncated)
	}
	return res, nil
}

// StartDownloadBatch queues several roots from one machine as a single
// download job: every root is pulled into one local tree and the
// browser receives one tar.gz (MachineTransferResult). fileName names
// the archive's root entry; an empty one falls back to the first
// root's basename.
func (m *TransferManager) StartDownloadBatch(machineID string, remotePaths []string, fileName string) (*Transfer, error) {
	machine, ok := m.machineForTransfer(machineID)
	if !ok {
		return nil, errors.New("machine not found")
	}
	if !machine.CarriesFiles() {
		return nil, fmt.Errorf("transfers are only available for ssh machines (this machine is %s)", machine.Protocol)
	}
	if len(remotePaths) == 0 {
		return nil, errors.New("nothing to download")
	}
	if m.sftp == nil {
		return nil, errors.New("sftp is not available")
	}
	paths := make([]string, 0, len(remotePaths))
	for _, p := range remotePaths {
		c := CleanRemotePath(p)
		if c == "/" {
			return nil, fmt.Errorf("refusing to download the filesystem root")
		}
		paths = append(paths, c)
	}
	if fileName == "" {
		fileName = path.Base(paths[0])
	}
	if err := os.MkdirAll(m.outDir(), 0o750); err != nil {
		return nil, err
	}

	id := uuid.NewString()
	t := &Transfer{
		ID:          id,
		MachineID:   machineID,
		Machine:     machine.Name,
		Kind:        "download",
		RemotePaths: paths,
		LocalFile:   filepath.Join(m.outDir(), id),
		FileName:    fileName,
		StartedAt:   time.Now().UTC(),
		status:      TransferRunning,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	m.register(t)

	m.run("ssh_machines_transfer_download", t, func() { m.runDownload(ctx, t) })
	return t, nil
}

// saveLocal writes one walked entry into the job's local tree.
func saveLocal(ctx context.Context, client sftpFS, base string, f FileSpec, t *Transfer, onBytes func(int64)) error {
	out, err := joinUnder(base, f.RelPath)
	if err != nil {
		return err
	}
	if f.Kind == specSymlink {
		target, lerr := client.ReadLink(f.AbsPath)
		if lerr != nil {
			return lerr
		}
		_ = os.Remove(out)
		return os.Symlink(target, out)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		return err
	}
	in, err := client.Open(f.AbsPath)
	if err != nil {
		return err
	}
	defer in.Close()
	fh, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	cerr := stream(ctx, fh, in, putOpts{
		OnBytes: onBytes,
		Gate:    t.waitIfPaused,
	})
	if cerr2 := fh.Close(); cerr == nil {
		cerr = cerr2
	}
	if cerr != nil {
		return cerr
	}
	if f.Mode != 0 {
		_ = os.Chmod(out, f.Mode.Perm())
	}
	if !f.ModTime.IsZero() {
		_ = os.Chtimes(out, f.ModTime, f.ModTime)
	}
	return nil
}

// joinUnder resolves rel against base and refuses anything that escapes
// it. Remote listings give single names per level, but a hostile or
// broken sshd could surface odd entries, and this is where a local
// write would turn into an arbitrary file creation.
func joinUnder(base, rel string) (string, error) {
	if rel == "" {
		return base, nil
	}
	if path.IsAbs(rel) {
		return "", fmt.Errorf("refusing absolute relpath %q under %s", rel, base)
	}
	p := filepath.Clean(filepath.Join(base, filepath.FromSlash(rel)))
	if p != base && !strings.HasPrefix(p, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing to write outside %s: %q", base, rel)
	}
	return p, nil
}
