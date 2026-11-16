// Package zmodem implements both directions of an rz/sz transfer over an
// already-open terminal channel: the receiver (3a, "sz" on the far side
// sends a file to the browser) and the sender (3b, "rz" on the far side
// takes a file from us).
//
// Why this exists in Go rather than in the browser: a finished download
// has to become one ordinary sshmachines.Transfer job, so the queue, the
// tar.gz /result endpoint and the SSE progress channel keep working with
// no new client-side plumbing.
//
// Frame format (ZMODEM Inter-Application File Transfer Protocol, as
// implemented by lrzsz and documented by the Tera Term wiki):
//
//	hex header     2a 2a 18 42  tt tt  f3 f3 f2 f2 f1 f1 f0 f0  c0..c3  CR LF [XON]
//	bin16 header   2a 18 41     tt    f3    f2    f1    f0    c1 c2
//	bin32 header   2a 18 43     tt    f3    f2    f1    f0    c1 c2 c3 c4
//
// tt = frame type, f3..f0 are the four header bytes in wire order; for
// numeric frames they carry a 32-bit position, little-endian (the flag
// and position indexes are deliberately reversed). Everything after the
// kind byte in a binary header, and every data subpacket byte, is
// ZDLE-escaped.
//
// The checksums, captured from the byte stream of a real lrzsz 0.12.21
// rather than read out of a description of it:
//
//   - CRC16 is transmitted high byte first, CRC32 low byte first;
//   - a subpacket checksum covers the payload *and* the trailer byte
//     (ZCRCE/ZCRCG/ZCRCQ/ZCRCW), not the payload alone — a sender that
//     leaves the trailer out gets a ZNAK for every block;
//   - a hex header may be closed by CR LF or by CR 8A (lrzsz's own
//     spelling), and both are accepted on the way in.
//
// The decoder therefore accepts either convention wherever they differ:
// one of them is wrong against real lrzsz, and the other is what this
// package's own fixtures have always produced.
package zmodem

import (
	"hash/crc32"
	"os"
	"strconv"
	"time"
)

// Control characters.
const (
	ZPAD   = 0x2a // '*' — begins every header
	ZDLE   = 0x18 // ^X  — data link escape (also ZCAN's value)
	ZDLEE  = 0x58 // 'X' — an escaped ZDLE
	ZBIN   = 0x41 // 'A' — binary header, CRC16
	ZHEX   = 0x42 // 'B' — hex header
	ZBIN32 = 0x43 // 'C' — binary header, CRC32

	XON  = 0x11
	XOFF = 0x13
)

// Subpacket enders, sent as ZDLE followed by the byte itself.
const (
	ZCRCE = 0x68 // 'h' last subpacket of this file
	ZCRCG = 0x69 // 'i' more subpackets follow in this frame
	ZCRCQ = 0x6a // 'j' sender waits for ZACK
	ZCRCW = 0x6b // 'k' sender waits for a response
)

// Frame types.
const (
	TypeZRQINIT    = 0x00
	TypeZRINIT     = 0x01
	TypeZSINIT     = 0x02
	TypeZACK       = 0x03
	TypeZFILE      = 0x04
	TypeZSKIP      = 0x05
	TypeZNAK       = 0x06
	TypeZABORT     = 0x07
	TypeZFIN       = 0x08
	TypeZRPOS      = 0x09
	TypeZDATA      = 0x0a
	TypeZEOF       = 0x0b
	TypeZFERR      = 0x0c
	TypeZCRC       = 0x0d
	TypeZCHALLENGE = 0x0e
	TypeZCOMPL     = 0x0f
	TypeZCAN       = 0x10
	TypeZFREECNT   = 0x11
	TypeZCOMMAND   = 0x12
	TypeZSTDERR    = 0x13
)

// Flags of the ZRINIT we answer with. CANFDX+CANOVIO say "I can receive
// while I speak and while I write", which is what lets the sender stream;
// CANFC32 advertises the 32-bit checksum (the receiver accepts either
// convention regardless — see verifySub).
const (
	flagCANFDX   = 0x01
	flagCANOVIO  = 0x02
	flagCANFC32  = 0x20
	initWithFlag = flagCANFDX | flagCANOVIO | flagCANFC32
)

// Header is one decoded ZMODEM frame header.
type Header struct {
	Type byte
	// Raw is the four wire-order header bytes (ZF3 ZF2 ZF1 ZF0 when the
	// frame means flags, ZP0 ZP1 ZP2 ZP3 when it means a position).
	Raw  [4]byte
	CRC  uint32
	Kind byte // ZBIN, ZHEX or ZBIN32
}

// Pos reads the header as a little-endian position.
func (h Header) Pos() uint32 {
	return uint32(h.Raw[0]) | uint32(h.Raw[1])<<8 | uint32(h.Raw[2])<<16 | uint32(h.Raw[3])<<24
}

// F0 is the flag byte that carries the receiver/sender capabilities.
func (h Header) F0() byte { return h.Raw[3] }

func posBytes(pos uint32) [4]byte {
	return [4]byte{byte(pos), byte(pos >> 8), byte(pos >> 16), byte(pos >> 24)}
}

// crc16Update is the XMODEM polynomial (0x1021, init 0, no reflection,
// no final xor) that ZMODEM uses for its 16-bit checksums. There is no
// hash/crc16 in the standard library, and the table would be 128 KiB for
// five lines of arithmetic. The incremental form exists for the sender's
// ZCRC answer, which has to run the checksum over a prefix of a file
// that may be several megabytes long without materialising it.
func crc16Update(prev uint16, p []byte) uint16 {
	crc := prev
	for _, b := range p {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func crc16Of(p []byte) uint16 { return crc16Update(0, p) }

// crc32Of is the IEEE checksum ZMODEM calls "32-bit CRC": init 0 and
// final xor 0xffffffff, which is exactly crc32.ChecksumIEEE.
func crc32Of(p []byte) uint32 { return crc32.ChecksumIEEE(p) }

// crc32Update continues a running 32-bit checksum. Go inverts at both
// ends of crc32.Update for the IEEE table, so the value carried between
// calls is already the final one: chaining it over a file in blocks
// yields exactly crc32Of over the same bytes, and a caller that stops
// early must not xor again.
func crc32Update(prev uint32, p []byte) uint32 { return crc32.Update(prev, crc32.IEEETable, p) }

// subChecksum is the checksum of one data subpacket: payload plus the
// trailer byte, which is what lrzsz computes (a payload-only checksum
// gets ZNAK'd — see the package comment).
func subChecksum(data []byte, ender byte, useCRC32 bool) uint32 {
	if useCRC32 {
		return crc32Of(append(append([]byte(nil), data...), ender))
	}
	return uint32(crc16Of(append(append([]byte(nil), data...), ender)))
}

// subVerifies is the decoder's side of the same rule, and it is
// deliberately wider than the encoder: implementations differ on whether
// the trailer joins the 16-bit checksum as well as on its byte order, so
// a byte that arrives is measured against every reading that a real
// sender might have meant. A corrupted block still misses all of them.
func subVerifies(data []byte, ender byte, useCRC32 bool, wire []byte) bool {
	if useCRC32 {
		if len(wire) != 4 {
			return false
		}
		v32 := crc32Of(data)
		v32e := crc32Of(append(append([]byte(nil), data...), ender))
		be, le := int32be(wire), int32le(wire)
		return v32 == be || v32 == le || v32e == be || v32e == le
	}
	if len(wire) != 2 {
		return false
	}
	want := uint32(wire[0])<<8 | uint32(wire[1])
	return uint32(crc16Of(data)) == want || uint32(crc16Of(append(append([]byte(nil), data...), ender))) == want
}

func int32be(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func int32le(b []byte) uint32 {
	return uint32(b[3])<<24 | uint32(b[2])<<16 | uint32(b[1])<<8 | uint32(b[0])
}

// maxSubpacket bounds one data subpacket. Real senders stay at or below
// 1024 bytes for CRC16 and use the receiver's buffer size otherwise; 64
// KiB is far above both while keeping a hostile stream from growing the
// scratch buffer without end.
const maxSubpacket = 64 << 10

// hexDigit is the ZMODEM hex alphabet. The spec asks for the digits as
// printed by lrzsz (lowercase); decoding accepts both cases and ignores
// bit 7, because a hex header traversing a 7-bit path has its MSB stripped.
func hexVal(c byte) (byte, bool) {
	switch {
	case c&0x7f >= '0' && c&0x7f <= '9':
		return c&0x7f - '0', true
	case c&0x7f >= 'a' && c&0x7f <= 'f':
		return c&0x7f - 'a' + 10, true
	case c&0x7f >= 'A' && c&0x7f <= 'F':
		return c&0x7f - 'A' + 10, true
	}
	return 0, false
}

const hexAlphabet = "0123456789abcdef"

// needsEscape is the set of bytes a sender must never put on the wire
// unescaped: the control characters that a communication channel eats.
// ESCCTL (asked for in ZSINIT) widens this to every byte below 0x20; the
// sender we talk to decides that, not us.
//
// 0x08, 0x88 and 0x98 are in here for a different reason: a receiver is
// allowed to read an eighth-bit-set control byte as its 7-bit form, so a
// raw 0x88 can be handed to lrzsz as a backspace and quietly replace a
// byte of the file. Escaping costs one extra byte on the wire; guessing
// wrong costs the file.
func needsEscape(c byte, escCtl bool) bool {
	switch c {
	case ZDLE, 0x98, 0x10, 0x90, 0x08, 0x88, XON, 0x91, XOFF, 0x93, 0x40, 0xc0, 0x0d, 0x8d:
		return true
	}
	if escCtl {
		return c&0x60 == 0
	}
	return false
}

// escapeBytes appends the ZDLE-encoded form of src to dst.
func escapeBytes(dst, src []byte, escCtl bool) []byte {
	for _, c := range src {
		if needsEscape(c, escCtl) {
			dst = append(dst, ZDLE, c^0x40)
			continue
		}
		dst = append(dst, c)
	}
	return dst
}

// unescape maps a byte that followed ZDLE back to the value the sender
// meant. 0x7f and 0xff are escaped with bit 7 cleared, so they are
// handled before the general rule.
func unescape(b byte) byte {
	switch b {
	case 0x7d: // escaped 0x7f
		return 0x7f
	case 0xbf: // escaped 0xff
		return 0xff
	}
	return b ^ 0x40
}

// hexHeader renders the 18-byte hex header plus its CR LF, and — for
// every type but ZACK and ZFIN — the trailing XON that releases spurious
// flow control.
func hexHeader(typ byte, raw [4]byte) []byte {
	var body [5]byte
	body[0] = typ
	copy(body[1:], raw[:])
	crc := crc16Of(body[:])
	out := make([]byte, 0, 21)
	out = append(out, ZPAD, ZPAD, ZDLE, ZHEX)
	for _, v := range body {
		out = append(out, hexAlphabet[v>>4], hexAlphabet[v&0x0f])
	}
	out = append(out,
		hexAlphabet[byte(crc>>12)&0x0f], hexAlphabet[byte(crc>>8)&0x0f],
		hexAlphabet[byte(crc>>4)&0x0f], hexAlphabet[byte(crc)&0x0f])
	out = append(out, '\r', '\n')
	if typ != TypeZACK && typ != TypeZFIN {
		out = append(out, XON)
	}
	return out
}

// hexPosHeader builds a hex header whose four bytes are a position.
func hexPosHeader(typ byte, pos uint32) []byte {
	return hexHeader(typ, posBytes(pos))
}

// binaryHeader renders a 16- or 32-bit-CRC binary header with an escaped
// body. Only the upload direction (rz) needs it today; it lives here so
// the encoder and the decoder cannot drift apart.
func binaryHeader(typ byte, raw [4]byte, useCRC32 bool) []byte {
	var body [5]byte
	body[0] = typ
	copy(body[1:], raw[:])
	out := make([]byte, 0, 13)
	if useCRC32 {
		sum := crc32Of(body[:])
		out = append(out, ZPAD, ZDLE, ZBIN32)
		out = escapeBytes(out, body[:], false)
		// Low byte first: this is what lrzsz puts on the wire, and what
		// the rz on the other end of a serial line computes against.
		out = escapeBytes(out, []byte{byte(sum), byte(sum >> 8), byte(sum >> 16), byte(sum >> 24)}, false)
		return out
	}
	sum := crc16Of(body[:])
	out = append(out, ZPAD, ZDLE, ZBIN)
	out = escapeBytes(out, body[:], false)
	out = escapeBytes(out, []byte{byte(sum >> 8), byte(sum)}, false)
	return out
}

// subpkgFrame appends one data subpacket to dst: the escaped payload, the
// ZDLE-quoted trailer and the checksum of both. The caller must have put
// the frame's header in dst already, because the trailer's width is the
// header's — a hex header means CRC16, exactly as the parser infers it.
func subpkgFrame(dst, data []byte, ender byte, useCRC32, escCtl bool) []byte {
	out := escapeBytes(dst, data, escCtl)
	out = append(out, ZDLE, ender)
	if useCRC32 {
		sum := crc32Of(append(append([]byte(nil), data...), ender))
		return escapeBytes(out, []byte{byte(sum), byte(sum >> 8), byte(sum >> 16), byte(sum >> 24)}, false)
	}
	sum := crc16Of(append(append([]byte(nil), data...), ender))
	return escapeBytes(out, []byte{byte(sum >> 8), byte(sum)}, false)
}

// zfileFrame announces one file. lrzsz's receiver reads the descriptor
// out of the subpacket and ignores the header's four bytes, but they are
// sent as the real sz sends them (all zero) so a stricter receiver that
// looks at the descriptor length finds something sane.
func zfileFrame(meta []byte) []byte {
	return subpkgFrame(hexHeader(TypeZFILE, [4]byte{}), meta, ZCRCW, false, false)
}

// dataFrame carries one block of a file. The trailer is ZCRCW while the
// sender wants an answer and ZCRCE for the last block of a file; the
// upload dialogue in sender.go stops and waits on every block, so it
// always asks.
func dataFrame(off uint32, data []byte, ender byte, useCRC32, escCtl bool) []byte {
	return subpkgFrame(binaryHeader(TypeZDATA, posBytes(off), useCRC32), data, ender, useCRC32, escCtl)
}

// fileMetaPacket is ZFILE's descriptor: the name, then the classic
// decimal-size / octal-mtime / octal-mode triple. The three trailing
// fields are what lrzsz 0.12.21 itself sends (file type, name length and
// total size); leaving them out would be legal, putting the same values
// in is what this build has actually seen accepted by a real rz.
func fileMetaPacket(name string, size int64, mode os.FileMode, when time.Time) []byte {
	meta := make([]byte, 0, len(name)+48)
	meta = append(meta, name...)
	meta = append(meta, 0)
	meta = strconv.AppendInt(meta, size, 10)
	meta = append(meta, ' ')
	meta = strconv.AppendInt(meta, when.Unix(), 8)
	meta = append(meta, ' ')
	meta = strconv.AppendInt(meta, int64(0o100000|uint32(mode&os.ModePerm)), 8)
	meta = append(meta, ' ', '0', ' ', '1', ' ')
	meta = strconv.AppendInt(meta, size, 10)
	return append(meta, 0)
}
