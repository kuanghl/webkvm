package zmodem

// The download direction: this end plays the part of `rz`, the remote
// runs `sz` and hands us its files.
//
// Why this lives in Go rather than in the browser: a finished download
// has to become one ordinary sshmachines.Transfer job, so the queue, the
// tar.gz /result endpoint and the SSE progress channel keep working with
// no new client-side plumbing.
//
// The framing rules, the watchdog and the promise about the user's
// keystrokes are the shared core in core.go; what is below is only the
// dialogue of a receiver.

import (
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

// Receiver is the ZMODEM receiver end of one terminal session: it plays
// the part of `rz`, but lands the bytes on the webkvm host instead of on
// the operator's desktop.
//
// One Receiver belongs to one terminal WebSocket. The pump hands it the
// remote's output (Feed) and the user's keystrokes (Keys) and forwards
// whatever comes back; the Receiver writes its own protocol frames
// straight to the pty.
type Receiver struct {
	*core

	cur       *os.File
	curPath   string
	curName   string
	curSize   int64
	offerSize int64
	curMode   os.FileMode
	curTime   time.Time

	// sizeBase is the bytes of the files that already landed.
	sizeBase  int64
	filesGot  int
	filesDone int
	badCRC    int
	files     []string
}

// New returns a receiver that writes its protocol frames to pty and
// lands files under opt.Dir.
func New(pty io.Writer, opt Options) *Receiver {
	r := &Receiver{}
	r.core = newCore(pty, opt, r)
	return r
}

// Start arms the session: it types the sz command into the pty and
// begins watching the reply. Nothing is swallowed until a real frame
// shows up, so a shell that answers "sz: command not found" stays
// perfectly usable.
func (r *Receiver) Start(paths []string, recursive bool) error {
	cmd, err := szCommand(paths, recursive)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.opt.Dir, 0o750); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.armed {
		return errf("a zmodem session is already running on this terminal")
	}
	r.armLocked()
	return r.writeFrame([]byte(cmd))
}

// ownsLine is the receiver's answer to the core's question: every phase
// from the first frame to the end of the batch belongs to the protocol.
func (r *Receiver) ownsLine() bool {
	switch r.phase {
	case phaseWaitInit, phaseWaitName, phaseWaitData, phaseData, phaseWaitEOF, phaseWaitFin:
		return true
	}
	return false
}

// resultLocked reports what landed. Files are host paths, in arrival
// order; the caller turns them into a job and then into names the
// browser may see.
func (r *Receiver) resultLocked(status Status, reason string, err error) Result {
	return Result{
		Status: status, Reason: reason, Err: err,
		Files: append([]string(nil), r.files...),
	}
}

// cleanupLocked closes whatever file was mid-download. A failed session
// leaves the partial bytes on disk for the caller to throw away, which
// is the same rule an interrupted SFTP download follows.
func (r *Receiver) cleanupLocked() { r.closeFileLocked() }

// event is the parser callback. It runs only from Feed's goroutine while
// holding the core's lock.
func (r *Receiver) event(e Event) {
	if r.closed {
		return
	}
	switch e.Kind {
	case evSub:
		r.sub(e)
	case evHeader:
		r.header(e.Header)
	case evAbort:
		if r.phase == phaseArmed {
			// Shell text that happened to contain five 0x18 bytes is not
			// a canceled transfer. Only a live session can be aborted.
			return
		}
		r.finishLocked(StatusError, "peer-abort", errf("the sender canceled the transfer"))
	}
}

func (r *Receiver) header(h Header) {
	switch h.Type {
	case TypeZRQINIT:
		// "I have something to send": announce readiness. This frame is
		// what turns the arm window into a live transfer.
		r.writeLock(hexHeader(TypeZRINIT, [4]byte{0, 0, 0, initWithFlag}))
		r.phase = phaseWaitInit
	case TypeZSINIT:
		r.writeLock(hexHeader(TypeZACK, posBytes(0)))
	case TypeZFILE:
		r.phase = phaseWaitName
	case TypeZDATA:
		r.startData(h)
	case TypeZEOF:
		r.endFile()
	case TypeZFIN:
		// The batch is over. Answer with ZFIN (no XON after it, per the
		// spec) and hand the terminal back; whatever the sender still
		// prints ("OO", the prompt) becomes output again.
		r.writeLock(hexHeader(TypeZFIN, [4]byte{}))
		r.finishLocked(StatusDone, "", nil)
	case TypeZABORT:
		r.finishLocked(StatusError, "sender-abort", errf("the sender aborted the batch"))
	case TypeZCAN:
		r.finishLocked(StatusCanceled, "peer-cancel", errf("the sender canceled"))
	default:
		// ZRINIT/ZACK/ZRPOS/ZSKIP/ZNAK/ZFERR/ZCOMPL/ZFREECNT/ZCOMMAND/
		// ZSTDERR/ZCRC/ZCHALLENGE: frames a sender may answer us with
		// that need no action here. A checksum request in particular is
		// only ever sent when the receiver asked for one.
		r.logf("ignoring frame %02x in %s", h.Type, phaseName(r.phase))
	}
}

// startData checks the offset the sender resumed from. Resuming is not
// supported, and a mismatch means the two ends have different ideas
// about a file being written.
func (r *Receiver) startData(h Header) {
	if r.cur == nil {
		r.finishLocked(StatusError, "data-without-file", errf("ZDATA before any ZFILE"))
		return
	}
	if uint64(h.Pos()) != uint64(r.curSize) {
		r.finishLocked(StatusError, "bad-offset",
			errf("sender resumed at %d but %s has %d bytes", h.Pos(), r.curName, r.curSize))
		return
	}
	r.phase = phaseData
}

func (r *Receiver) sub(e Event) {
	switch e.Header.Type {
	case TypeZFILE:
		r.fileMeta(e.Data)
		return
	case TypeZSINIT:
		return // the attention sequence is not needed on a live pty
	case TypeZDATA:
	default:
		return
	}
	if r.cur == nil {
		r.finishLocked(StatusError, "data-without-file", errf("subpacket before any ZFILE"))
		return
	}
	if r.opt.Cancelled != nil && r.opt.Cancelled() {
		r.finishLocked(StatusCanceled, "canceled", nil)
		return
	}
	if !e.CRCOK {
		r.badCRC++
		if r.badCRC > maxBadCRC {
			r.finishLocked(StatusError, "bad-checksum",
				errf("%s: %d subpackets failed the checksum", r.curName, r.badCRC))
			return
		}
		// Ask for the block again, from where the file actually ends.
		r.writeLock(hexHeader(TypeZNAK, [4]byte{}))
		r.writeLock(hexHeader(TypeZRPOS, posBytes(uint32(r.curSize))))
		return
	}
	r.badCRC = 0
	n, err := r.cur.Write(e.Data)
	r.curSize += int64(n)
	r.progress()
	if err != nil {
		r.finishLocked(StatusError, "write-failed", errf("write %s: %w", r.curName, err))
		return
	}
	switch e.Ender {
	case ZCRCE:
		r.phase = phaseWaitEOF
	case ZCRCW, ZCRCQ:
		// The sender is waiting for this; without it the transfer stops.
		r.writeLock(hexHeader(TypeZACK, posBytes(uint32(r.curSize))))
	}
}

// fileMeta turns a ZFILE subpacket into the file about to be written.
func (r *Receiver) fileMeta(data []byte) {
	if r.phase != phaseWaitName {
		r.finishLocked(StatusError, "zfile-out-of-order",
			errf("ZFILE metadata seen while %s", phaseName(r.phase)))
		return
	}
	o, extra, err := parseOffer(data)
	if err != nil {
		r.finishLocked(StatusError, "bad-zfile", err)
		return
	}
	if r.filesGot >= maxDownloadFiles {
		r.finishLocked(StatusError, "too-many-files",
			errf("the sender passed %d files in one batch, the limit is %d",
				r.filesGot, maxDownloadFiles))
		return
	}
	if len(extra) > 0 {
		r.logf("batch announces %d more names (each arrives as its own ZFILE)", len(extra))
	}
	dst, err := safeJoin(r.opt.Dir, o.Name)
	if err != nil {
		r.finishLocked(StatusError, "bad-name", err)
		return
	}
	if err := os.MkdirAll(path.Dir(dst), 0o750); err != nil {
		r.finishLocked(StatusError, "mkdir-failed", err)
		return
	}
	// A name that lands twice is renamed rather than overwritten: the
	// directory is per job, so a repeat means the sender listed the same
	// name twice (sz -r over two trees that match one glob).
	if dst, err = avoidClash(dst); err != nil {
		r.finishLocked(StatusError, "name-clash", err)
		return
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		r.finishLocked(StatusError, "open-failed", errf("create %s: %w", dst, err))
		return
	}
	r.closeFileLocked()
	r.cur, r.curPath, r.curName = f, dst, o.Name
	r.curSize, r.offerSize = 0, o.Size
	r.curMode, r.curTime = o.Mode, o.ModTime
	r.filesGot++
	r.progress()
	// ZRPOS 0 says "send it from the beginning", which is the only
	// offset this build can take.
	r.writeLock(hexHeader(TypeZRPOS, posBytes(0)))
	r.phase = phaseWaitData
}

// endFile closes the current file, applies what the offer said about
// permissions and time, and asks for the next one.
func (r *Receiver) endFile() {
	if r.cur == nil {
		r.finishLocked(StatusError, "eof-without-file", errf("ZEOF without an open file"))
		return
	}
	size, dst, name, mode, when := r.curSize, r.curPath, r.curName, r.curMode, r.curTime
	r.closeFileLocked()
	if mode != 0 {
		_ = os.Chmod(dst, mode)
	}
	if !when.IsZero() {
		_ = os.Chtimes(dst, when, when)
	}
	r.files = append(r.files, dst)
	r.filesDone++
	r.sizeBase += size
	if r.opt.OnFile != nil {
		r.opt.OnFile(FileEvent{Name: name, Path: dst, Size: size, Mode: mode})
	}
	r.writeLock(hexHeader(TypeZRINIT, [4]byte{0, 0, 0, initWithFlag}))
	r.phase = phaseWaitFin
}

// avoidClash appends " (2)", " (3)"… to a name that already landed, so a
// repeated name in one batch keeps every copy instead of losing one.
//
// Every answer but "nothing is there" is fatal. A permission or I/O
// fault makes the probe fail the same way an absent file does, and
// guessing "free" would let the O_TRUNC at the call site erase a file
// nobody selected; running out of suffixes means the same thing. Lstat,
// not Stat: a dangling symlink occupies a name just as a file does.
func avoidClash(dst string) (string, error) {
	free := func(p string) (bool, error) {
		if _, err := os.Lstat(p); err != nil {
			if os.IsNotExist(err) {
				return true, nil
			}
			return false, errf("probe %s: %w", p, err)
		}
		return false, nil
	}
	ok, err := free(dst)
	if err != nil {
		return "", err
	}
	if ok {
		return dst, nil
	}
	ext := path.Ext(dst)
	stem := strings.TrimSuffix(dst, ext)
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if empty, err := free(cand); err != nil {
			return "", err
		} else if empty {
			return cand, nil
		}
	}
	return "", errf("%s: 998 files already share that name", dst)
}

func (r *Receiver) closeFileLocked() {
	if r.cur != nil {
		_ = r.cur.Close()
		r.cur = nil
		r.curPath, r.curName = "", ""
		r.curSize, r.offerSize = 0, 0
		r.curMode, r.curTime = 0, time.Time{}
	}
}

func (r *Receiver) progress() {
	if r.opt.OnProgress == nil {
		return
	}
	r.opt.OnProgress(Progress{
		Current:   r.curName,
		Done:      r.sizeBase + r.curSize,
		Total:     r.sizeBase + r.offerSize,
		FilesDone: r.filesDone,
		FilesGot:  r.filesGot,
	})
}

// szCommand builds the remote command line.
//
// The paths arrive from the browser, so they are quoted rather than
// interpolated (see quoteArg); a leading "-" would be parsed by sz as an
// option even after quoting, so it is anchored with "./" instead.
// Anything that still cannot survive a shell is refused.
func szCommand(paths []string, recursive bool) (string, error) {
	if len(paths) == 0 {
		return "", errf("no paths to download")
	}
	quoted := make([]string, 0, len(paths))
	total := 0
	for _, p := range paths {
		if p == "" || strings.ContainsRune(p, '\n') || strings.ContainsRune(p, '\r') ||
			strings.ContainsRune(p, 0) {
			return "", errf("path %q cannot be sent to a shell", p)
		}
		if strings.HasPrefix(p, "-") {
			p = "./" + p
		}
		q := quoteArg(p)
		total += len(q)
		if total > 3000 {
			// N_TTY's line discipline truncates a canonical line well
			// before this; refusing beats a silently missing file.
			return "", errf("too many paths for one sz command")
		}
		quoted = append(quoted, q)
	}
	cmd := "sz -q"
	if recursive {
		cmd += " -r"
	}
	return cmd + " " + strings.Join(quoted, " ") + "\r", nil
}

// shellQuote wraps one argument in single quotes, so every character in
// it — spaces, semicolons, backticks, globbers — arrives at the program
// exactly as typed.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// quoteArg quotes one sz argument the way a shell user means it.
//
// Literal runs go through shellQuote; only "*" and "?" step outside the
// quotes, because sz does not expand wildcards itself and a dialog that
// asks for "paths on the machine" is otherwise a trap: "/var/log/*.log"
// would be handed over as a filename and fail with "cannot open". Nothing
// else is ever bare, so ";" "`" "$" and a stray quote cannot leave the
// quoted run they arrived in.
//
// The cost is the mirror image: a file whose name really contains "*" or
// "?" cannot be asked for here — exactly what happens if you type that name
// at a shell prompt, which is the thing this dialog stands in for. Squaring
// that circle would mean inventing an escape syntax the hint text would
// have to teach. Brackets stay literal too: an unterminated "[" is a common
// filename fragment, and deciding where a class ends is not worth the
// surface it opens.
func quoteArg(s string) string {
	if !strings.ContainsAny(s, "*?") {
		return shellQuote(s)
	}
	var out strings.Builder
	i := 0
	for i < len(s) {
		if c := s[i]; c == '*' || c == '?' {
			out.WriteByte(c)
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] != '*' && s[j] != '?' {
			j++
		}
		out.WriteString(shellQuote(s[i:j]))
		i = j
	}
	return out.String()
}
