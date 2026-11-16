package zmodem

import (
	"fmt"
)

// eventKind names what the parser found in the byte stream.
type eventKind int

const (
	// evHeader is a complete frame header whose CRC checked.
	evHeader eventKind = iota
	// evSub is one data subpacket of the frame evHeader started.
	evSub
	// evAbort is a run of ZCAN long enough to mean "the peer gave up".
	evAbort
)

// Event is one parser output. Data is only valid during the callback.
// Anything the parser does not consume is returned by Feed, not emitted.
type Event struct {
	Kind   eventKind
	Header Header
	Data   []byte
	Ender  byte
	// CRCOK reports a subpacket checksum mismatch; the session decides
	// whether to ask for a retransmit or to give up.
	CRCOK bool
}

// Parser states. One switch, no table, so a reader can see which byte
// may follow which.
const (
	stSeek    = iota
	stPad2    // saw ZPAD, expecting ZPAD (hex) or ZDLE (binary)
	stHexZDLE // saw ZPAD ZPAD, expecting ZDLE
	stHexKind // saw ZPAD ZPAD ZDLE, expecting 'B'
	stHexBody // collecting the 14 hex characters
	stHexEOL  // discarding the CR LF [XON] that closes a hex header
	stBinKind // saw ZPAD ZDLE, expecting 'A' or 'C'
	stBinBody // collecting the 5 escaped header bytes
	stBinCRC  // collecting the escaped header CRC
	stSub     // collecting an escaped data subpacket
	stSubCRC  // collecting the escaped subpacket CRC
)

// Parser turns the raw remote byte stream into ZMODEM framed events.
//
// It is incremental on purpose: the terminal hands it whatever a read
// returned, so a header, an escape pair or a CRC may straddle chunks.
// Nothing is buffered beyond one frame, and bytes that are not protocol
// come back as the return value of Feed — that is what keeps the shell
// visible while a session is armed but sz has not answered yet.
type Parser struct {
	st   int
	emit func(Event)

	// cand holds the bytes of a header candidate that is not yet
	// confirmed, so a false start (a '*' in shell output followed by a
	// control byte) can be replayed as text instead of vanishing.
	cand   []byte
	hexbuf [14]byte
	body   [5]byte
	n      int
	hdr    Header
	sub    []byte
	esc    bool
	// ender is the subpacket trailer that closed the current subpacket.
	ender byte
	// crcBuf/crcn collect the CRC bytes that follow a header or a
	// subpacket.
	crcBuf [4]byte
	crcn   int
	crcPos int
	// canRun counts consecutive ZCAN bytes seen outside a frame; a run
	// of five is the sender's abort signal.
	canRun int

	// out is the text to forward for the current Feed call.
	out  []byte
	halt bool

	// Err is set by the first undecodable frame. A stream that only
	// contains shell text never sets it.
	Err error
}

func NewParser(emit func(Event)) *Parser {
	return &Parser{emit: emit, sub: make([]byte, 0, 4096)}
}

// Feed parses chunk and returns the bytes that must reach the terminal:
// shell output, replayed false candidates, and whatever followed the
// last protocol byte when the session stopped mid-chunk.
//
// The returned slice is reused by the next Feed call.
func (p *Parser) Feed(chunk []byte) []byte {
	p.out = p.out[:0]
	for i := 0; i < len(chunk); i++ {
		if p.Err != nil || p.halt {
			p.out = append(p.out, chunk[i:]...)
			p.halt = false
			return p.out
		}
		p.byte(chunk[i])
	}
	return p.out
}

// Halt tells the parser the session is over: the remaining bytes of the
// current chunk belong to the shell, not to the protocol.
func (p *Parser) Halt() { p.halt = true }

// send hands one event to whoever owns the parser. A parser built
// without a callback still decodes: the bytes it declines to forward are
// the whole story for a caller that only wants the framing.
func (p *Parser) send(e Event) {
	if p.emit != nil {
		p.emit(e)
	}
}

// ClearErr forgets a decode error so a false alarm in shell output
// cannot poison the frames that follow it.
func (p *Parser) ClearErr() {
	p.Err = nil
	p.st = stSeek
	p.cand = p.cand[:0]
	p.sub = p.sub[:0]
	p.esc = false
}

// text queues a byte that belongs to the shell.
func (p *Parser) text(c byte) { p.out = append(p.out, c) }

// dropCand replays a failed header candidate as terminal text.
func (p *Parser) dropCand() {
	p.out = append(p.out, p.cand...)
	p.cand = p.cand[:0]
}

func (p *Parser) fail(format string, args ...any) {
	if p.Err == nil {
		p.Err = fmt.Errorf("zmodem: "+format, args...)
	}
	p.st = stSeek
	// The candidate bytes go back to the screen: shell text that looked
	// like a header and then failed its checksum is output the user must
	// still see (a hexdump of a binary file is enough to hit this).
	p.dropCand()
	p.sub = p.sub[:0]
	p.esc = false
	p.canRun = 0
}

// abandon resets a candidate that turned out not to be a header and
// reprocesses the offending byte from the seek state.
func (p *Parser) abandon(c byte) {
	p.canRun = 0
	p.dropCand()
	p.st = stSeek
	p.byte(c)
}

func (p *Parser) byte(c byte) {
	switch p.st {
	case stSeek:
		switch {
		case c == ZPAD:
			p.cand = append(p.cand[:0], c)
			p.st = stPad2
		case c == ZDLE:
			// Counted only in the seek state: the ZDLE of a real header
			// is consumed by stPad2, so a genuine frame is never
			// mistaken for an abort run.
			p.canRun++
			if p.canRun >= 5 {
				// The run means "the peer gave up". The CANs queued as
				// text so far are protocol noise, not output, so they go
				// back off the forward buffer.
				if n := p.canRun - 1; n <= len(p.out) {
					p.out = p.out[:len(p.out)-n]
				}
				p.canRun = 0
				p.send(Event{Kind: evAbort})
				return
			}
			p.text(c)
		default:
			p.canRun = 0
			p.text(c)
		}

	case stPad2:
		switch c {
		case ZPAD:
			p.cand = append(p.cand, c)
			p.st = stHexZDLE
		case ZDLE:
			p.cand = append(p.cand, c)
			p.st = stBinKind
		default:
			p.abandon(c)
		}

	case stHexZDLE:
		if c != ZDLE {
			p.abandon(c)
			return
		}
		p.cand = append(p.cand, c)
		p.st = stHexKind

	case stHexKind:
		if c != ZHEX {
			p.abandon(c)
			return
		}
		p.cand = append(p.cand, c)
		p.canRun = 0
		p.hdr = Header{Kind: ZHEX}
		p.n = 0
		p.st = stHexBody

	case stBinKind:
		if c != ZBIN && c != ZBIN32 {
			p.abandon(c)
			return
		}
		p.cand = append(p.cand, c)
		p.canRun = 0
		p.hdr = Header{Kind: c}
		p.n = 0
		p.crcPos = 0
		p.st = stBinBody

	case stHexBody:
		if _, ok := hexVal(c); !ok {
			p.abandon(c)
			return
		}
		p.cand = append(p.cand, c)
		p.hexbuf[p.n] = c
		p.n++
		if p.n == 10 {
			for i := 0; i < 5; i++ {
				hi, _ := hexVal(p.hexbuf[i*2])
				lo, _ := hexVal(p.hexbuf[i*2+1])
				p.body[i] = hi<<4 | lo
			}
			p.hdr.Type = p.body[0]
			copy(p.hdr.Raw[:], p.body[1:])
			return
		}
		if p.n < 14 {
			return
		}
		var got uint32
		for i := 10; i < 14; i++ {
			v, _ := hexVal(p.hexbuf[i])
			got = got<<4 | uint32(v)
		}
		p.hdr.CRC = got
		if crc16Of(p.body[:]) != uint16(got) {
			// fail() replays cand, so the bytes stay on screen.
			p.fail("hex header crc mismatch (type %02x)", p.hdr.Type)
			return
		}
		if !frameKnown(p.hdr.Type) {
			p.fail("unknown frame type %02x", p.hdr.Type)
			return
		}
		p.cand = p.cand[:0]
		p.st = stHexEOL
		p.send(Event{Kind: evHeader, Header: p.hdr})

	case stHexEOL:
		// A hex header is always closed by CR LF, and every type but
		// ZACK and ZFIN then sends XON to release spurious flow control.
		// Those are framing, not output: forwarding them would print a
		// blank line for every handshake frame.
		switch c {
		case '\r', '\n', XON:
			return
		}
		// A hex header that announces data — ZFILE's pathname packet —
		// is followed by an escaped subpacket, whatever the header's own
		// encoding was. Without this the name bytes would be mistaken
		// for shell output and every download would die at the first
		// file.
		if p.hdr.hasData() {
			p.st = stSub
			p.sub = p.sub[:0]
			p.esc = false
			p.byte(c)
			return
		}
		p.st = stSeek
		p.byte(c)

	case stBinBody:
		v, ok := p.escaped(c)
		if !ok {
			return
		}
		if p.n < 5 {
			p.body[p.n] = v
			if p.n == 0 {
				p.hdr.Type = v
			} else {
				p.hdr.Raw[p.n-1] = v
			}
		}
		p.n++
		if p.n == 5 {
			p.n = 0
			p.crcn = 2
			if p.hdr.Kind == ZBIN32 {
				p.crcn = 4
			}
			p.crcPos = 0
			p.st = stBinCRC
		}

	case stBinCRC:
		v, ok := p.escaped(c)
		if !ok {
			return
		}
		p.crcBuf[p.crcPos] = v
		p.crcPos++
		if p.crcPos == p.crcn {
			p.crcPos = 0
			p.st = stSeek
			p.finishBinHeader()
		}

	case stSub:
		if p.esc {
			p.esc = false
			switch c {
			case ZCRCE, ZCRCG, ZCRCQ, ZCRCW:
				p.ender = c
				p.crcn = 2
				if p.hdr.Kind == ZBIN32 {
					p.crcn = 4
				}
				p.crcPos = 0
				p.st = stSubCRC
				return
			}
			p.pushSub(unescape(c))
			return
		}
		if c == ZDLE {
			p.esc = true
			return
		}
		// A raw flow character here is line noise, never content: a
		// sender escapes these, so the decoded value must reach the file
		// untouched. Filtering after unescaping would silently delete
		// 0x11 and 0x13 from every binary download.
		switch c {
		case XON, 0x91, XOFF, 0x93:
			return
		}
		p.pushSub(c)

	case stSubCRC:
		v, ok := p.escaped(c)
		if !ok {
			return
		}
		p.crcBuf[p.crcPos] = v
		p.crcPos++
		if p.crcPos == p.crcn {
			p.crcPos = 0
			p.finishSub()
		}
	}
}

// pushSub appends one decoded payload byte. Filtering happens on the raw
// wire bytes in the caller, never here: by this point 0x11 and 0x13 are
// file content again.
func (p *Parser) pushSub(c byte) {
	if len(p.sub) >= maxSubpacket {
		p.fail("subpacket longer than %d bytes", maxSubpacket)
		return
	}
	p.sub = append(p.sub, c)
}

// escaped consumes one byte of an escaped run, returning the value the
// sender meant. Flow characters inside a frame are swallowed silently.
func (p *Parser) escaped(c byte) (byte, bool) {
	switch c {
	case XON, 0x91, XOFF, 0x93:
		return 0, false
	}
	if p.esc {
		p.esc = false
		return unescape(c), true
	}
	if c == ZDLE {
		p.esc = true
		return 0, false
	}
	return c, true
}

func (p *Parser) finishBinHeader() {
	// lrzsz transmits a 32-bit checksum low byte first; the description
	// most implementations were written against says high byte first.
	// Both are accepted, because one of them is what the machine on the
	// other end of the wire actually sends.
	if p.hdr.Kind == ZBIN32 {
		want := crc32Of(p.body[:])
		if want != int32be(p.crcBuf[:]) && want != int32le(p.crcBuf[:]) {
			p.fail("binary header crc32 mismatch (type %02x)", p.hdr.Type)
			return
		}
	} else {
		want := uint32(crc16Of(p.body[:]))
		if want != uint32(p.crcBuf[0])<<8|uint32(p.crcBuf[1]) {
			p.fail("binary header crc16 mismatch (type %02x)", p.hdr.Type)
			return
		}
	}
	if !frameKnown(p.hdr.Type) {
		p.fail("unknown frame type %02x", p.hdr.Type)
		return
	}
	if p.hdr.hasData() {
		p.st = stSub
		p.sub = p.sub[:0]
	}
	p.send(Event{Kind: evHeader, Header: p.hdr})
}

func (p *Parser) finishSub() {
	sub := append([]byte(nil), p.sub...)
	p.sub = p.sub[:0]
	ok := subVerifies(sub, p.ender, p.crcn == 4, p.crcBuf[:p.crcn])
	// A ZCRCG says "more subpackets follow in this frame", so the bytes
	// after it are payload, not a new header — that is what makes a file
	// full of '*' bytes safe. Any other trailer hands the stream back to
	// header seeking.
	if p.ender == ZCRCG {
		p.st = stSub
	} else {
		p.st = stSeek
	}
	p.send(Event{Kind: evSub, Header: p.hdr, Data: sub, Ender: p.ender, CRCOK: ok})
}

// frameKnown is the set of types this build understands.
func frameKnown(t byte) bool {
	switch t {
	case TypeZRQINIT, TypeZRINIT, TypeZSINIT, TypeZACK, TypeZFILE, TypeZSKIP,
		TypeZNAK, TypeZABORT, TypeZFIN, TypeZRPOS, TypeZDATA, TypeZEOF,
		TypeZFERR, TypeZCRC, TypeZCHALLENGE, TypeZCOMPL, TypeZCAN,
		TypeZFREECNT, TypeZCOMMAND, TypeZSTDERR:
		return true
	}
	return false
}

// hasData reports whether a frame type carries data subpackets. Without
// this the parser could not tell payload bytes from a new header, so the
// table is what keeps a payload byte of ZPAD from starting a frame.
func (h Header) hasData() bool {
	switch h.Type {
	case TypeZSINIT, TypeZFILE, TypeZDATA, TypeZCOMMAND, TypeZFREECNT:
		return true
	}
	return false
}
