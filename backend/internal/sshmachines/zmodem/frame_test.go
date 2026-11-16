package zmodem

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// The standard check value: CRC-16/XMODEM of "123456789" is 0x31C3 and
// CRC-32/ISO-HDLC of it is 0xCBF43926. Both ZMODEM checksums are those
// two algorithms, so these vectors pin the choice down.
func TestChecksumsAreTheStandardOnes(t *testing.T) {
	probe := []byte("123456789")
	if got := crc16Of(probe); got != 0x31c3 {
		t.Errorf("crc16Of = %#04x, want 0x31c3 (CRC-16/XMODEM)", got)
	}
	if got := crc32Of(probe); got != 0xcbf43926 {
		t.Errorf("crc32Of = %#08x, want 0xcbf43926 (CRC-32/IEEE)", got)
	}
}

func TestHexHeaderLayout(t *testing.T) {
	h := hexHeader(TypeZRINIT, [4]byte{0, 0, 0, initWithFlag})
	// ZPAD ZPAD ZDLE 'B' + 14 hex chars + CR LF + XON.
	if want := 4 + 14 + 3; len(h) != want {
		t.Fatalf("hex header is %d bytes, want %d: % q", len(h), want, h)
	}
	if !bytes.HasPrefix(h, []byte{ZPAD, ZPAD, ZDLE, ZHEX}) {
		t.Errorf("wrong lead bytes: % q", h[:4])
	}
	if h[18] != '\r' || h[19] != '\n' || h[20] != XON {
		t.Errorf("wrong framing tail: % q", h[18:])
	}
	body := string(h[4:18])
	if strings.ToLower(body) != body {
		t.Errorf("the spec asks for lowercase hex digits, got %q", body)
	}
	if got := string(h[4:6]); got != "01" {
		t.Errorf("type digits = %q, want 01 (ZRINIT)", got)
	}
	// ZACK and ZFIN must not push an XON: it would release flow control
	// the sender never asked about and shows as a stray NUL in some
	// emulators.
	for _, typ := range []byte{TypeZACK, TypeZFIN} {
		z := hexHeader(typ, posBytes(3))
		if len(z) != 20 {
			t.Fatalf("frame %02x is %d bytes, want 20 (no XON)", typ, len(z))
		}
		if z[18] != '\r' || z[19] != '\n' {
			t.Errorf("frame %02x tail = % q, want CR LF", typ, z[18:])
		}
	}
}

func TestHexPosHeaderIsLittleEndian(t *testing.T) {
	// The position is written least-significant byte first while the
	// flag bytes of the same four slots are ordered the other way — the
	// trap that makes hand-written ZMODEM encoders wrong.
	const pos = uint32(0x01020304)
	ev := decodeAll(t, hexPosHeader(TypeZRPOS, pos))
	if len(ev) != 1 || ev[0].Kind != evHeader {
		t.Fatalf("events = %+v, want one header", ev)
	}
	h := ev[0].Header
	if h.Type != TypeZRPOS {
		t.Errorf("type = %02x", h.Type)
	}
	if h.Pos() != pos {
		t.Errorf("Pos() = %#x, want %#x", h.Pos(), pos)
	}
	if h.Raw != [4]byte{0x04, 0x03, 0x02, 0x01} {
		t.Errorf("raw = % x, want 04 03 02 01", h.Raw)
	}
}

func TestNeedsEscapeSet(t *testing.T) {
	// Every one of these would be eaten by a communication channel or by
	// the line discipline, so a sender must never put one on the wire.
	for _, c := range []byte{ZDLE, 0x98, 0x10, 0x90, 0x08, 0x88, XON, 0x91, XOFF, 0x93, 0x40, 0xc0, 0x0d, 0x8d} {
		if !needsEscape(c, false) {
			t.Errorf("%#02x must be escaped", c)
		}
	}
	for _, c := range []byte{'a', 0x00, 0x7f, 0xff, '*', 0x60, 0xe0} {
		if needsEscape(c, false) {
			t.Errorf("%#02x must not be escaped", c)
		}
	}
	// ESCCTL widens the set to every control byte, MSB included.
	if !needsEscape(0x01, true) || !needsEscape(0x81, true) {
		t.Error("ESCCTL must escape the whole low block")
	}
	if needsEscape('a', true) {
		t.Error("ESCCTL must not escape ordinary text")
	}
}

// TestEveryByteSurvivesEscaping replays all 256 byte values through the
// encoder and then through the parser, both as one stream and cut at
// every single offset: a chunk boundary that falls inside an escape
// pair, inside a checksum, or between a trailer's ZDLE and the trailer
// itself must not change the outcome.
func TestEveryByteSurvivesEscaping(t *testing.T) {
	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	for _, crc32On := range []bool{false, true} {
		for _, ender := range []byte{ZCRCE, ZCRCG, ZCRCQ, ZCRCW} {
			in := zframe(TypeZDATA, posBytes(0), crc32On, subpkg{data: payload, ender: ender})
			label := fmt.Sprintf("crc32=%v ender=%02x", crc32On, ender)
			if got := payloadOf(drain(t, in)); !bytes.Equal(got, payload) {
				t.Fatalf("%s whole stream: payload lost (%d bytes, first diff at %d)",
					label, len(got), firstDiff(got, payload))
			}
			for split := 1; split < len(in); split++ {
				got := payloadOf(drain(t, in[:split], in[split:]))
				if !bytes.Equal(got, payload) {
					t.Fatalf("%s at split %d: first diff at %d", label, split, firstDiff(got, payload))
				}
			}
		}
	}
}

// drain feeds each chunk in turn and collects the events, copies
// included, so a test can assert on a whole exchange.
func drain(t *testing.T, chunks ...[]byte) []Event {
	t.Helper()
	var ev []Event
	p := NewParser(func(e Event) {
		if e.Kind == evSub {
			e.Data = append([]byte(nil), e.Data...)
		}
		ev = append(ev, e)
	})
	for _, c := range chunks {
		p.Feed(c)
	}
	if p.Err != nil {
		t.Fatalf("unexpected parse error: %v", p.Err)
	}
	return ev
}

func payloadOf(ev []Event) []byte {
	var out []byte
	for _, e := range ev {
		if e.Kind == evSub {
			out = append(out, e.Data...)
		}
	}
	return out
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}

// TestSubpacketTooLongIsRefused keeps a hostile stream from growing the
// scratch buffer without end.
func TestSubpacketTooLongIsRefused(t *testing.T) {
	big := make([]byte, maxSubpacket+1)
	p := NewParser(nil)
	p.Feed(zframe(TypeZDATA, posBytes(0), false, subpkg{data: big, ender: ZCRCE}))
	if p.Err == nil {
		t.Fatal("an oversized subpacket must be refused")
	}
}

// TestFlowCharactersBetweenFramePartsAreIgnored: a raw XON/XOFF that
// arrives *between* protocol elements is flow control from the line, not
// file content. Inside an escape pair it would be the escaped value
// itself, so those positions must not be filtered — which is why the
// noise goes around the elements, never between a ZDLE and its byte.
func TestFlowCharactersBetweenFramePartsAreIgnored(t *testing.T) {
	data := []byte("hello")
	crc := crc16Of(data)
	header := binaryHeader(TypeZDATA, posBytes(0), false)
	body := escapeBytes(nil, data, false)
	trailer := append([]byte{ZDLE, ZCRCE}, escapeBytes(nil, []byte{byte(crc >> 8), byte(crc)}, false)...)

	var in []byte
	in = append(in, XOFF, 0x91)
	in = append(in, header...)
	in = append(in, XON, 0x93)
	in = append(in, body...)
	in = append(in, XOFF, 0x91)
	in = append(in, trailer...)
	in = append(in, XON)

	ev := decodeAll(t, in)
	var got []byte
	hdr := 0
	for _, e := range ev {
		if e.Kind == evHeader {
			hdr++
		}
		if e.Kind == evSub {
			got = append(got, e.Data...)
		}
	}
	if hdr != 1 {
		t.Errorf("headers decoded = %d, want 1", hdr)
	}
	if string(got) != "hello" {
		t.Errorf("payload = %q, want hello", got)
	}
}

// TestTextBetweenFramesIsForwarded proves a header candidate that is not
// a frame comes back as terminal output instead of vanishing: the bytes
// Feed returns are exactly what the pump must still show.
func TestFalseCandidatesAreForwardedAsText(t *testing.T) {
	in := []byte("echo *18hi\r\n" + "**not-a-frame\r\n" + "*\x18z\r\n")
	p := NewParser(nil)
	got := p.Feed(in)
	if p.Err != nil {
		t.Fatalf("text must never set an error: %v", p.Err)
	}
	if string(got) != string(in) {
		t.Errorf("forwarded %q, want the original %q", got, in)
	}
}

// TestShellTextThatLooksLikeAFrameIsNotEaten: a hexdump containing a
// plausible header with a wrong checksum must return every byte and
// report the failure to the caller instead of silently swallowing
// screen output.
func TestShellTextThatLooksLikeAFrameIsNotEaten(t *testing.T) {
	before := "0000000  "
	// A real hex header shape, CRC deliberately wrong ("ffff" cannot
	// match the body's checksum).
	bad := "**\x18B0100000023ffff\r\n\x11"
	after := "\nrest of the line\n"
	p := NewParser(nil)
	got := p.Feed([]byte(before + bad + after))
	if p.Err == nil {
		t.Fatal("a header that fails its checksum must be reported")
	}
	want := before + bad + after
	if string(got) != want {
		t.Errorf("forwarded %q, want everything (%q)", got, want)
	}
	// ClearErr lets the next chunk be parsed again.
	p.ClearErr()
	if got := p.Feed([]byte("still working\n")); string(got) != "still working\n" {
		t.Errorf("after ClearErr, forwarded %q", got)
	}
}

// TestHeaderAfterZCRCGStaysPayload: inside one frame, ZCRCG means "more
// data follows", so a '*' or a whole fake header in the middle of the
// file must land in the file, not in the header seeker.
func TestHeaderAfterZCRCGStaysPayload(t *testing.T) {
	part1 := []byte("ABCD")
	part2 := []byte{ZPAD, ZPAD, ZDLE, ZHEX, '0', '1', '0', '0', '0', '0', '0', '0', '2', '3', 'f', 'f', 'f', 'f', '\r', '\n'}
	part3 := []byte("EF")
	frame := zframe(TypeZDATA, posBytes(0), false,
		subpkg{data: part1, ender: ZCRCG},
		subpkg{data: part2, ender: ZCRCG},
		subpkg{data: part3, ender: ZCRCE})
	var got []byte
	hdrs := 0
	for _, e := range decodeAll(t, frame) {
		if e.Kind == evHeader {
			hdrs++
		}
		if e.Kind == evSub {
			got = append(got, e.Data...)
		}
	}
	if hdrs != 1 {
		t.Fatalf("headers decoded = %d, want the single ZDATA", hdrs)
	}
	if !bytes.Equal(got, append(append(part1, part2...), part3...)) {
		t.Errorf("payload = % q", got)
	}
}

// TestBinaryHeaderWith32BitCRC decodes the frame shape a CANFC32
// receiver gets from lrzsz.
func TestBinaryHeaderWith32BitCRC(t *testing.T) {
	ev := decodeAll(t, zframe(TypeZDATA, posBytes(1024), true, subpkg{data: []byte("x"), ender: ZCRCE}))
	if ev[0].Kind != evHeader || ev[0].Header.Pos() != 1024 {
		t.Fatalf("events = %+v", ev)
	}
	if ev[0].Header.Kind != ZBIN32 {
		t.Errorf("kind = %02x, want ZBIN32", ev[0].Header.Kind)
	}
	if !ev[1].CRCOK {
		t.Error("the checksum of a well-formed CRC32 frame must verify")
	}
}

// TestSubpacketChecksumConventions: senders differ on whether the
// trailer byte joins a 32-bit subpacket checksum. Both must verify, and
// a damaged payload must not.
func TestSubpacketChecksumConventions(t *testing.T) {
	data := []byte("payload")
	// sub32 renders one CRC32 subpacket with an explicitly chosen
	// checksum, so the two conventions are tested independently of the
	// header's own.
	sub32 := func(sum uint32) []byte {
		out := binaryHeader(TypeZDATA, posBytes(0), true)
		out = escapeBytes(out, data, false)
		out = append(out, ZDLE, ZCRCE)
		return escapeBytes(out,
			[]byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)}, false)
	}
	dataOnly := crc32Of(data)
	withEnder := crc32Of(append(append([]byte(nil), data...), ZCRCE))
	for _, tc := range []struct {
		name string
		sum  uint32
		ok   bool
	}{
		{"data only", dataOnly, true},
		{"data and trailer", withEnder, true},
		{"mismatched", 0xa5a5a5a5, false},
	} {
		ev := decodeAll(t, sub32(tc.sum))
		if len(ev) != 2 || ev[1].Kind != evSub {
			t.Fatalf("%s: events = %+v, want header + subpacket", tc.name, ev)
		}
		if ev[1].CRCOK != tc.ok {
			t.Errorf("%s: CRCOK = %v, want %v", tc.name, ev[1].CRCOK, tc.ok)
		}
		if string(ev[1].Data) != string(data) {
			t.Errorf("%s: payload = %q", tc.name, ev[1].Data)
		}
	}
}

// ---- shared test builders (the sender side of the wire) ----

type subpkg struct {
	data  []byte
	ender byte
}

// zframe renders a binary frame the way a sender puts it on the wire:
// ZPAD ZDLE kind, five escaped header bytes, the header checksum, then
// each subpacket with its escaped payload, trailer and checksum.
func zframe(typ byte, raw [4]byte, crc32On bool, subs ...subpkg) []byte {
	out := binaryHeader(typ, raw, crc32On)
	for _, s := range subs {
		out = escapeBytes(out, s.data, false)
		out = append(out, ZDLE, s.ender)
		out = escapeBytes(out, subCRC(s, crc32On), false)
	}
	return out
}

// hexFrame renders a hex header plus, when the type carries one, an
// escaped subpacket after the CR LF. That is the shape of lrzsz's ZFILE:
// the header in hex, the pathname packet in binary.
func hexFrame(typ byte, raw [4]byte, subs ...subpkg) []byte {
	out := hexHeader(typ, raw)
	for _, s := range subs {
		out = escapeBytes(out, s.data, false)
		out = append(out, ZDLE, s.ender)
		out = escapeBytes(out, subCRC(s, false), false)
	}
	return out
}

func subCRC(s subpkg, crc32On bool) []byte {
	if crc32On {
		v := crc32Of(s.data)
		return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
	}
	v := crc16Of(s.data)
	return []byte{byte(v >> 8), byte(v)}
}

// decodeAll feeds a whole stream and keeps every event (the Data slice
// is only valid during the callback, so it is copied).
func decodeAll(t *testing.T, in []byte) []Event {
	t.Helper()
	return drain(t, in)
}
