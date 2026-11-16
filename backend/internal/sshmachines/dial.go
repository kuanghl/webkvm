package sshmachines

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
)

// Host-key trust model (webkvm precedent, cf. backupstore V13-BCK-05):
// a machine with an EMPTY HostKey has NOT been trusted yet — dialing it
// fails with ErrHostKeyNotPinned carrying the presented fingerprint so
// the UI can run the accept-and-pin flow. Once pinned, every dial is
// STRICT: a different key is ErrHostKeyChanged (possible reprovision or
// MITM) and is refused. There is no trust-on-first-use and no
// insecure-ignore path.
var (
	ErrHostKeyNotPinned = errors.New("ssh host key not pinned")
	ErrHostKeyChanged   = errors.New("ssh host key changed")
)

// HostKeyError is the typed host-key failure. Presented/
// PresentedFull describe the key the REMOTE just showed; Expected is
// the pinned value (only for ErrHostKeyChanged).
type HostKeyError struct {
	Err           error
	PresentedType string
	Presented     string // "SHA256:…"
	PresentedFull string // "ssh-ed25519 SHA256:…"
	Expected      string // pinned value (ErrHostKeyChanged)
}

func (e *HostKeyError) Error() string {
	if errors.Is(e.Err, ErrHostKeyNotPinned) {
		return fmt.Sprintf("ssh host key not pinned yet: the server presented %s", e.PresentedFull)
	}
	return fmt.Sprintf("ssh host key no longer matches the pinned fingerprint: presented %s, pinned %s", e.PresentedFull, e.Expected)
}

func (e *HostKeyError) Unwrap() error { return e.Err }

// Dial opens an SSH connection to the machine using its stored
// credentials, enforcing the strict host-key policy above. timeout 0
// means the default 15s.
func Dial(m *Machine, sec MachineSecret, timeout time.Duration) (*ssh.Client, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	authMethods, err := sshAuthMethods(m, sec)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            m.User,
		Auth:            authMethods,
		HostKeyCallback: strictHostKeyCallback(m),
		Timeout:         timeout,
	}
	addr := net.JoinHostPort(m.Host, strconv.Itoa(m.Port))
	conn, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		var hke *HostKeyError
		if errors.As(err, &hke) {
			return nil, hke
		}
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	return conn, nil
}

// strictHostKeyCallback implements the pinned-fingerprint check.
func strictHostKeyCallback(m *Machine) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		presented := ssh.FingerprintSHA256(key)
		full := key.Type() + " " + presented
		if m.HostKey == "" {
			return &HostKeyError{
				Err:           ErrHostKeyNotPinned,
				PresentedType: key.Type(),
				Presented:     presented,
				PresentedFull: full,
			}
		}
		if FingerprintPart(m.HostKey) == presented {
			return nil
		}
		return &HostKeyError{
			Err:           ErrHostKeyChanged,
			PresentedType: key.Type(),
			Presented:     presented,
			PresentedFull: full,
			Expected:      m.HostKey,
		}
	}
}

// sshAuthMethods builds the auth methods from the stored credential.
// ssh.Password first (when both are set) mirrors what a human ssh
// client would try, and key auth never falls back to a stale password.
func sshAuthMethods(m *Machine, sec MachineSecret) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	switch m.AuthType {
	case AuthPassword:
		if sec.Password == "" {
			return nil, fmt.Errorf("machine %q is configured for password auth but has no stored password", m.Name)
		}
		methods = append(methods, ssh.Password(sec.Password))
	case AuthKey:
		if sec.SSHKey == "" {
			return nil, fmt.Errorf("machine %q is configured for key auth but has no stored private key", m.Name)
		}
		signer, err := ParsePrivateKey(sec.SSHKey, sec.SSHKeyPassphrase)
		if err != nil {
			return nil, err
		}
		methods = append(methods, ssh.PublicKeys(signer))
	default:
		return nil, fmt.Errorf("machine %q has no supported auth type %q", m.Name, m.AuthType)
	}
	return methods, nil
}

// ParsePrivateKey parses an OpenSSH or PEM private key, using the
// passphrase only when the key is encrypted (a wrong passphrase on a
// plain key is treated as "no passphrase" by x/crypto, so we only
// retry with it when the plain parse failed).
func ParsePrivateKey(pem, passphrase string) (ssh.Signer, error) {
	key := []byte(pem)
	if passphrase != "" {
		if signer, err := ssh.ParsePrivateKeyWithPassphrase(key, []byte(passphrase)); err == nil {
			return signer, nil
		}
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	return signer, nil
}

// PublicFingerprint computes the "ssh-… SHA256:…" fingerprint of the
// public half of a stored private key. Used for UI display only.
func PublicFingerprint(sec MachineSecret) (string, error) {
	if sec.SSHKey == "" {
		return "", nil
	}
	signer, err := ParsePrivateKey(sec.SSHKey, sec.SSHKeyPassphrase)
	if err != nil {
		return "", err
	}
	return signer.PublicKey().Type() + " " + ssh.FingerprintSHA256(signer.PublicKey()), nil
}
