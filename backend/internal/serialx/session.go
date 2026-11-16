// Package serialx is the webkvm port of uniterm's serial session
// (backend/session/serial_session.go): open a serial port via
// go.bug.st/serial (v1.7.1, same version as uniterm) and bridge it to
// the WebSocket terminal.
//
// Ported: normalizeNewlines VERBATIM (device \r handling, incl. the
// double-\r and trailing-\r special cases), the 16 KiB read loop, read
// error reporting and graceful close.
//
// Deliberately NOT ported (P2 in the plan): line mode / flow control,
// per-port data-bits/parity/stop-bit configuration (v1 uses the 8N1
// default — by far the common case) and zmodem passthrough (reserved
// as a readLoop hook in the plan).
package serialx

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"go.bug.st/serial"

	"webkvm/internal/safego"
)

// Session is one open serial port. It implements io.Reader (with
// newline normalization applied) and io.Writer for the terminal pump.
type Session struct {
	mu        sync.Mutex
	port      serial.Port
	closed    bool
	closeCh   chan struct{}
	waiter    chan struct{} // closed when data arrives / session ends
	overflow  []byte
	closeOnce sync.Once

	OnClose func(error)
}

// Open opens the device at the given baud rate (8N1) and starts the
// read loop. device must be a path like /dev/ttyUSB0 (or COMn on
// Windows).
func Open(device string, baudRate int) (*Session, error) {
	if device == "" {
		return nil, errors.New("serial: device not set")
	}
	if baudRate <= 0 {
		baudRate = 115200
	}
	mode := &serial.Mode{
		BaudRate: baudRate,
		DataBits: 8,
		StopBits: serial.OneStopBit,
		Parity:   serial.NoParity,
	}
	port, err := serial.Open(device, mode)
	if err != nil {
		return nil, fmt.Errorf("serial open %s: %w", device, err)
	}
	s := &Session{port: port, closeCh: make(chan struct{})}
	go func() {
		defer safego.Recover("serialx_read_loop")
		s.readLoop()
	}()
	return s, nil
}

// ListPorts returns the device names of the serial ports the backend
// can see (for the UI's device picker).
func ListPorts() ([]string, error) {
	ports, err := serial.GetPortsList()
	if err != nil {
		return nil, err
	}
	return ports, nil
}

func (s *Session) readLoop() {
	buf := make([]byte, 16384)
	for {
		n, err := s.port.Read(buf)
		if n > 0 {
			s.push(normalizeNewlines(buf[:n]))
		}
		if err != nil {
			if err != io.EOF {
				s.push([]byte("\r\n\x1b[31m[Serial read error: " + err.Error() + "]\x1b[0m\r\n"))
			} else {
				s.push([]byte("\r\n\x1b[90m[Serial device disconnected.]\x1b[0m\r\n"))
			}
			s.finish()
			return
		}
	}
}

// maxOverflowBytes is the same ceiling the telnet session puts on its
// read buffer: the consumer is the WebSocket pump, and a tab that stops
// draining must not turn a fast serial line (921600 baud is ~90 KB/s)
// into unbounded memory while the pump runs out its write deadline. The
// newest bytes win, because that is what a terminal shows.
const maxOverflowBytes = 8 << 20

// push hands data to a blocked Read (or buffers it).
func (s *Session) push(data []byte) {
	s.mu.Lock()
	s.overflow = append(s.overflow, data...)
	if over := len(s.overflow) - maxOverflowBytes; over > 0 {
		s.overflow = append(s.overflow[:0], s.overflow[over:]...)
	}
	w := s.waiter
	s.waiter = nil
	s.mu.Unlock()
	if w != nil {
		close(w)
	}
}

// Read implements io.Reader. It blocks until the read loop delivers
// data or the session closes. normalizeNewlines output may be longer
// than the input (lone \r becomes \r\n), so excess bytes queue in
// overflow and are drained first.
func (s *Session) Read(p []byte) (int, error) {
	for {
		s.mu.Lock()
		if len(s.overflow) > 0 {
			n := copy(p, s.overflow)
			s.overflow = s.overflow[n:]
			s.mu.Unlock()
			return n, nil
		}
		if s.closed {
			s.mu.Unlock()
			return 0, io.EOF
		}
		w := make(chan struct{})
		s.waiter = w
		s.mu.Unlock()
		select {
		case <-w:
			// data arrived; loop
		case <-s.closeCh:
			s.mu.Lock()
			hasData := len(s.overflow) > 0
			s.closed = true
			s.mu.Unlock()
			if !hasData {
				return 0, io.EOF
			}
		}
	}
}

// Resize is a no-op for serial links (there is no window to resize);
// it exists so a serial session satisfies the terminal bridge interface.
func (s *Session) Resize(cols, rows int) error { return nil }

// Write implements io.Writer.
func (s *Session) Write(p []byte) (int, error) {
	s.mu.Lock()
	port := s.port
	s.mu.Unlock()
	if port == nil {
		return 0, errors.New("serial: not connected")
	}
	return port.Write(p)
}

// Close tears the session down. Idempotent.
func (s *Session) Close() error {
	s.finish()
	return nil
}

func (s *Session) finish() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		port := s.port
		s.port = nil
		s.closed = true
		w := s.waiter
		s.waiter = nil
		s.mu.Unlock()
		close(s.closeCh)
		if w != nil {
			close(w)
		}
		if port != nil {
			_ = port.Close()
		}
		if s.OnClose != nil {
			s.OnClose(nil)
		}
	})
}

// normalizeNewlines converts lone \r to \r\n so carriage returns from
// serial devices produce proper line breaks in the terminal. Ported
// VERBATIM from uniterm (serial_session.go):
//
//	\r\n is kept as-is; a \r followed by another \r (double Enter) or a
//	trailing \r at the end of the chunk is passed through unchanged, so
//	pressing Enter on an empty prompt does not add a blank line.
func normalizeNewlines(data []byte) []byte {
	out := make([]byte, 0, len(data)+16)
	for i := 0; i < len(data); i++ {
		b := data[i]
		if b == '\r' {
			// Check if followed by \n (keep as-is)
			if i+1 < len(data) && data[i+1] == '\n' {
				out = append(out, b)
			} else if i+1 < len(data) && data[i+1] == '\r' {
				// Double \r (double Enter): just pass through, don't add extra newline
				out = append(out, b)
			} else if i+1 >= len(data) {
				// Trailing \r at end of data: this is likely an empty Enter
				out = append(out, b)
			} else {
				// Lone \r not at end, convert to \r\n
				out = append(out, '\r', '\n')
			}
		} else {
			out = append(out, b)
		}
	}
	return out
}
