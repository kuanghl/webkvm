package zmodem

import (
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Receiver phases. The names are the protocol's own vocabulary so a
// reader who knows ZMODEM can follow the transitions in session.go
// without a translation layer.
const (
	phaseIdle = iota
	// phaseArmed: the command was typed and the stream is being
	// watched for the first frame. Keystrokes are buffered here so a
	// missing lrzsz does not cost the user their typing.
	phaseArmed
	// phaseWaitInit: ZRINIT was sent; the sender should answer with
	// ZFILE (or ZSINIT first, or ZFIN if it has nothing to send).
	phaseWaitInit
	phaseWaitName // ZFILE header seen, its metadata subpacket is next
	phaseWaitData // ZRPOS sent, ZDATA expected
	phaseData     // receiving the current file's subpackets
	phaseWaitEOF  // ZCRCE seen, ZEOF expected
	phaseWaitFin  // ZRINIT sent after ZEOF: another file, or ZFIN

	// Sender phases (this end plays `sz`, the remote runs `rz`). The
	// arm window is shared with the receiver: until the first frame the
	// phase is still phaseArmed.
	//
	// phaseSendGreet: the peer greeted us (ZRQINIT) and we answered, but
	// its ZRINIT — the frame that really starts the batch — is still out
	// there somewhere.
	phaseSendGreet
	phaseSendPos  // ZFILE offered, ZRPOS expected
	phaseSendData // one block in flight, its ZACK expected
	phaseSendNext // ZEOF sent: the next ZRINIT means another file or goodbye

	phaseDone
	phaseFailed
	phaseCanceled
)

func phaseName(p int) string {
	switch p {
	case phaseIdle:
		return "idle"
	case phaseArmed:
		return "armed"
	case phaseWaitInit:
		return "wait-init"
	case phaseWaitName:
		return "wait-name"
	case phaseWaitData:
		return "wait-data"
	case phaseData:
		return "data"
	case phaseWaitEOF:
		return "wait-eof"
	case phaseWaitFin:
		return "wait-fin"
	case phaseSendGreet:
		return "send-greet"
	case phaseSendPos:
		return "send-pos"
	case phaseSendData:
		return "send-data"
	case phaseSendNext:
		return "send-next"
	case phaseDone:
		return "done"
	case phaseFailed:
		return "failed"
	case phaseCanceled:
		return "canceled"
	}
	return "unknown"
}

// offer is what a ZFILE subpacket announces.
type offer struct {
	Name    string
	Size    int64
	Mode    os.FileMode
	ModTime time.Time
	HasMode bool
	HasTime bool
}

// parseOffer decodes the ZFILE metadata subpacket:
//
//	PATHNAME NUL [LENGTH " " MOD-DATE " " MODE] NUL
//
// LENGTH is decimal, MOD-DATE and MODE are octal (MODE carries the file
// type bits, so it is masked to permissions). A batch may list more than
// one name before the metadata of the first; the rest are returned so
// the caller can say how many files the sender intends to move.
func parseOffer(data []byte) (offer, []string, error) {
	fields := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	if len(fields) == 0 || fields[0] == "" {
		return offer{}, nil, errf("ZFILE without a name")
	}
	o := offer{Name: fields[0]}
	var extra []string
	for _, f := range fields[1:] {
		if f == "" {
			continue
		}
		parts := strings.Fields(f)
		if looksLikeMetadata(parts) {
			if v, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
				o.Size = v
			}
			if len(parts) > 1 {
				if v, err := strconv.ParseInt(parts[1], 8, 64); err == nil {
					o.ModTime, o.HasTime = time.Unix(v, 0).UTC(), true
				}
			}
			if len(parts) > 2 {
				if v, err := strconv.ParseUint(parts[2], 8, 32); err == nil {
					o.Mode, o.HasMode = os.FileMode(v)&os.ModePerm, true
				}
			}
			continue
		}
		extra = append(extra, f)
	}
	return o, extra, nil
}

// looksLikeMetadata tells a size/date/mode triple from another filename
// in a multi-name ZFILE: the first token must be a plain decimal number.
func looksLikeMetadata(parts []string) bool {
	if len(parts) == 0 {
		return false
	}
	if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
		return false
	}
	for _, p := range parts[1:] {
		if !numericField(p) {
			return false
		}
	}
	return true
}

// numericField reports whether a token is a number in either of the two
// bases the descriptor mixes: the length is decimal, the date and mode
// are octal, and lrzsz's own sz puts the trailing total back in decimal.
// A digit-only token is a number in one of those; anything else is part
// of a name. Reading only the octal half would make a size like 4800
// look like a filename, and the whole triple — including the length the
// progress bar needs — would be thrown away.
func numericField(p string) bool {
	if p == "" {
		return false
	}
	if _, err := strconv.ParseUint(p, 8, 64); err == nil {
		return true
	}
	_, err := strconv.ParseUint(p, 10, 64)
	return err == nil
}

// safeJoin maps a name the sender offered onto a path under dir.
//
// sz sends what the remote shell had: "dir/sub/f" for `sz -r`, a bare
// name for `sz f`, and — depending on the build — sometimes an absolute
// path. The landing tree is always rooted at dir:
//
//   - a name that climbs with ".." is refused outright. lrzsz never
//     produces one, so accepting it would only paper over a hostile or
//     broken sender, and silently renaming "a/../../b" to "b" would make
//     the queue lie about what the user downloaded;
//   - a leading "/" is dropped rather than refused, because "sz
//     /etc/hosts" is an ordinary download and stripping the root keeps
//     it both honest (the shape the remote named) and contained;
//   - after that the result is re-derived from a rooted Clean and
//     re-checked with filepath.Rel, so the guarantee comes from
//     verification rather than from trusting the filter above.
func safeJoin(dir, name string) (string, error) {
	if name == "" {
		return "", errf("empty file name")
	}
	if strings.ContainsRune(name, 0) {
		return "", errf("refusing odd file name %q", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", errf("refusing %q: it climbs out of the transfer directory", name)
		}
	}
	clean := path.Clean("/" + strings.TrimPrefix(name, "/"))
	out := path.Join(dir, strings.TrimPrefix(clean, "/"))
	// rel is checked against the two shapes that would escape: the dir
	// itself and a path starting with a real ".." component. A Linux
	// file simply named "..\x" or "..foo" is neither, and must be kept.
	if rel, err := filepath.Rel(dir, out); err != nil || rel == "." ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errf("path %q escapes the transfer directory", name)
	}
	return out, nil
}
