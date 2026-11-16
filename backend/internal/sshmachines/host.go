// The built-in host card is the one machine that lives on this box: its
// "dial" is the local /bin/login, so its credential management is a
// reduced copy of the SSH machine's — an account and a password, stored
// in the same 0600 companion file, and no host key to pin (a getty
// login has no key half).
package sshmachines

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// UpdateHostRequest is the only mutation the built-in host card accepts:
// the account its terminal logs in as and the credential behind it. Name,
// host, protocol and id are fixed — the card IS this machine, so the
// request carries none of them.
type UpdateHostRequest struct {
	User     string
	AuthType string // password | none ("" means none)
	Notes    string
	// Secret is the stored credential. nil in password mode keeps the
	// password already on disk (there must be one to keep); any value in
	// none mode is ignored, because manual login stores nothing.
	Secret *SecretInput
}

// HostMachine returns the built-in host card the way the API serves it:
// the persisted record when one exists, the synthetic base otherwise,
// with the display flags read from the secrets file.
func (s *Store) HostMachine() *Machine {
	s.mu.Lock()
	var m *Machine
	if stored, ok := s.machines[HostMachineID]; ok {
		cp := *stored
		m = &cp
	} else {
		m = SyntheticHost()
	}
	s.mu.Unlock()

	// A secrets read that fails leaves the flags false: the card still
	// renders, it just does not claim to store a password.
	if sm, err := s.readSecrets(); err == nil {
		if c, ok := sm[HostMachineID]; ok {
			m.HasPassword = c.Password != ""
		}
	}
	return m
}

// UpdateHost saves the built-in host card's account and credential. It
// is the one write path that may create the record with id
// HostMachineID; Create refuses the id and Delete keeps doing so, so the
// card can be configured but never turned into a regular machine.
func (s *Store) UpdateHost(req UpdateHostRequest) (*Machine, error) {
	a := strings.ToLower(strings.TrimSpace(req.AuthType))
	if a == "" {
		a = AuthNone
	}
	if a != AuthNone && a != AuthPassword {
		return nil, fmt.Errorf("the host card supports %s or %s authentication (got %q)", AuthPassword, AuthNone, req.AuthType)
	}
	user := strings.TrimSpace(req.User)
	if a == AuthPassword {
		if user == "" {
			return nil, errors.New("a login user is required for automatic login")
		}
		if req.Secret != nil && req.Secret.Password == "" {
			return nil, errors.New("a password is required for automatic login")
		}
	}
	// "Keep the current password" (nil secret in password mode) needs
	// something to keep. The secrets file is read outside s.mu, the same
	// discipline as Update: it is its own file with its own writer.
	if a == AuthPassword && req.Secret == nil {
		sm, err := s.readSecrets()
		if err != nil {
			return nil, fmt.Errorf("check stored credentials: %w", err)
		}
		if sm[HostMachineID].Password == "" {
			return nil, errors.New("a password is required for automatic login")
		}
	}

	s.mu.Lock()
	base := s.machines[HostMachineID]
	if base == nil {
		base = SyntheticHost()
		base.CreatedAt = time.Now().UTC()
	}
	m := *base
	m.User = user
	m.AuthType = a
	m.Notes = strings.TrimSpace(req.Notes)
	m.UpdatedAt = time.Now().UTC()
	s.machines[HostMachineID] = &m
	if err := s.saveLocked(); err != nil {
		if base == nil {
			delete(s.machines, HostMachineID)
		} else {
			s.machines[HostMachineID] = base
		}
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()

	switch {
	case a == AuthPassword && req.Secret != nil:
		if err := s.replaceSecret(HostMachineID, MachineSecret{Password: req.Secret.Password}); err != nil {
			return nil, fmt.Errorf("save secrets: %w", err)
		}
	case a == AuthNone:
		// Manual login: clear whatever was stored.
		if err := s.dropSecret(HostMachineID); err != nil {
			return nil, fmt.Errorf("clear secrets: %w", err)
		}
	}
	return s.HostMachine(), nil
}
