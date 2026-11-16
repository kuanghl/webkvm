//go:build windows

package telnet

import "net"

// enableOOBInline is a no-op on Windows: SO_OOBINLINE has no portable
// equivalent and the SYNCH Data Mark byte is tolerated by the IAC
// filter (it consumes "IAC DM" pairs silently).
func enableOOBInline(conn net.Conn) {}
