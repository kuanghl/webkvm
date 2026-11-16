package sshmachines

import (
	"io"
	"net"

	"github.com/pkg/sftp"

	"webkvm/internal/safego"
)

// The built-in host card is not a dial-able machine: it IS the machine
// webkvm runs on. Serving its files through a real *sftp.Client keeps a
// single code path for paths, modes, extensions and the atomic putFile —
// a hand-written local adapter of the sftpFS seam would be a second
// implementation to keep in sync, and it would be the one that writes to
// the host as root.
//
// pkg/sftp ships the server side of the protocol and it operates on the
// real filesystem, so the pair is wired over an in-memory pipe: the
// client end is pooled, leased and reaped exactly like a dialed session.
//
// This is a root-privileged surface, which is why nothing reaches it
// through the machine store: the API layer answers it only for admins
// (see api.machineForRequest).

// localPair is one end of the in-process session: closing it tears the
// whole pipe down, so neither side can be left blocked on the other.
type localPair struct {
	cli  *sftp.Client
	a, b net.Conn
}

func (p *localPair) Close() error {
	// Both pipe ends go first: a synchronous net.Pipe write from
	// cli.Close() would otherwise wait for a server that has stopped
	// reading, and the registry's teardown has to be able to finish.
	_ = p.b.Close()
	_ = p.a.Close()
	return p.cli.Close()
}

// dialLocalSFTP returns a client rooted at the host filesystem. The
// server's working directory is pinned to "/" so the browser opens on the
// filesystem root whatever directory the webkvm process was started in.
func dialLocalSFTP() (*sftp.Client, io.Closer, error) {
	a, b := net.Pipe()
	srv, err := sftp.NewServer(b, sftp.WithServerWorkingDirectory("/"))
	if err != nil {
		_ = a.Close()
		_ = b.Close()
		return nil, nil, err
	}
	// Serve does the version exchange, so it must be running before the
	// client writes its INIT: on an unbuffered pipe either side waiting
	// for the other would deadlock the dial.
	errc := make(chan error, 1)
	go func() {
		defer safego.Recover("local_sftp_server")
		errc <- srv.Serve()
	}()
	cli, err := sftp.NewClientPipe(a, a)
	if err != nil {
		_ = a.Close()
		_ = b.Close()
		<-errc
		return nil, nil, err
	}
	return cli, &localPair{cli: cli, a: a, b: b}, nil
}
