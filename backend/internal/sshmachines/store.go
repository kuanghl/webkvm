// Package sshmachines implements the "SSH Machines" feature: named remote
// hosts reachable over SSH, Telnet, or a serial link, with a terminal
// bridge and an SFTP file browser on top of SSH.
//
// Persistence follows the groups.json precedent: one JSON file holds the
// machine records (ssh-machines.json) and a second, 0600-permission file
// (ssh-machine-secrets.json) holds the credentials. Splitting the two keeps
// the list API — and any future export of the machine store — from ever
// being able to leak passwords or private keys.
package sshmachines

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Protocols a machine can be defined with. Mosh is accepted as a record
// (so an operator can pre-configure a machine) but the terminal bridge
// refuses to open one: the transport is not part of this build.
const (
	ProtocolSSH    = "ssh"
	ProtocolTelnet = "telnet"
	ProtocolSerial = "serial"
	ProtocolMosh   = "mosh"
	// ProtocolLocal labels the built-in webkvm-host pseudo machine
	// (the old "Host Terminal"). It is deliberately NOT in
	// knownProtocols: no user-created machine may claim it.
	ProtocolLocal = "local"
)

// HostMachineID is the stable id of the built-in local machine.
const HostMachineID = "host"

var knownProtocols = map[string]bool{
	ProtocolSSH:    true,
	ProtocolTelnet: true,
	ProtocolSerial: true,
	ProtocolMosh:   true,
}

// AuthTypes per protocol: ssh needs credentials, telnet may use
// auto-login or manual, serial has none.
const (
	AuthPassword = "password"
	AuthKey      = "key"
	AuthNone     = "none"
)

// Machine is the persisted record for one remote host. It never carries
// secret material — see MachineSecret and the secrets file.
type Machine struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`  // ssh | telnet | serial | mosh | local (builtin only)
	Host     string `json:"host"`      // host/IP; for serial: the device path
	Port     int    `json:"port"`      // tcp port; for serial: baud rate
	User     string `json:"user"`      // login user ("" allowed for telnet manual mode)
	AuthType string `json:"auth_type"` // password | key | none
	Notes    string `json:"notes,omitempty"`
	Builtin  bool   `json:"builtin,omitempty"` // the built-in host card (persisted once configured)
	// HostKey is the pinned host-key fingerprint ("ssh-ed25519 SHA256:…"
	// or bare "SHA256:…"). Empty until the operator accepts the key via
	// the test-and-pin flow; dials are then STRICT — see dial.go.
	HostKey   string    `json:"host_key,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Display-only flags, populated on read from the secrets file.
	// Never persisted into ssh-machines.json.
	HasPassword bool `json:"has_password"`
	HasKey      bool `json:"has_key"`
	// KeyFingerprint is the "ssh-… SHA256:…" fingerprint of the public
	// half of the stored private key, for display in the UI.
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
}

// MachineSecret is one machine's credential blob. Stored 0600, returned
// only to the dial/test paths, never serialized into an API response.
type MachineSecret struct {
	Password         string `json:"password,omitempty"`
	SSHKey           string `json:"ssh_key,omitempty"`
	SSHKeyPassphrase string `json:"ssh_key_passphrase,omitempty"`
}

// SyntheticHost builds the base record for the webkvm host itself (the
// terminal that used to live at #/host-console). The record is injected
// for admins at the API layer; once the operator saves an account for it
// (UpdateHost) a persisted copy joins the record file and the copy in the
// map wins over this base. Every other mutating endpoint still refuses
// the id (see api/sshmachines.go).
func SyntheticHost() *Machine {
	name, err := os.Hostname()
	if err != nil || name == "" {
		name = "webkvm host"
	}
	return &Machine{
		ID:        HostMachineID,
		Name:      name,
		Protocol:  ProtocolLocal,
		Host:      "localhost",
		AuthType:  AuthNone,
		Builtin:   true,
		CreatedAt: time.Time{},
	}
}

// CarriesFiles is the protocol half of the file-plane rule: an ssh
// machine, or this very machine under the built-in host card. Telnet and
// serial machines have no filesystem to browse.
//
// Authorization is not part of it. The host card reads and writes the
// disk with webkvm's own privileges, so only the API layer — the one
// place that knows who asked — may answer it, to admins.
func (m *Machine) CarriesFiles() bool {
	return m.Protocol == ProtocolSSH || (m.Builtin && m.Protocol == ProtocolLocal)
}

// SecretInput is the credential payload of a create/update request. A nil
// input on update means "leave the current secrets untouched".
type SecretInput struct {
	Password         string
	SSHKey           string
	SSHKeyPassphrase string
}

func (s *SecretInput) toSecret() MachineSecret {
	if s == nil {
		return MachineSecret{}
	}
	return MachineSecret{
		Password:         s.Password,
		SSHKey:           s.SSHKey,
		SSHKeyPassphrase: s.SSHKeyPassphrase,
	}
}

// CreateRequest / UpdateRequest share their shape; Update adds the ID.
type CreateRequest struct {
	Name     string
	Protocol string
	Host     string
	Port     int
	User     string
	AuthType string
	Notes    string
	Secret   *SecretInput
	HostKey  string // optional operator pre-pin (advanced)
}

type UpdateRequest = CreateRequest

func (req CreateRequest) toMachine(existing *Machine) (*Machine, error) {
	if existing == nil {
		return nil, errors.New("internal: nil existing machine")
	}
	m := *existing
	m.Name = strings.TrimSpace(req.Name)
	if m.Name == "" {
		return nil, errors.New("name is required")
	}
	if len(m.Name) > 120 {
		return nil, errors.New("name is too long (max 120 characters)")
	}
	m.Protocol, m.AuthType = normalizeProtocolAuth(req.Protocol, req.AuthType)
	if m.Protocol == ProtocolLocal {
		return nil, errors.New("the local protocol is reserved for the built-in host machine")
	}
	if !knownProtocols[m.Protocol] {
		return nil, fmt.Errorf("unknown protocol %q", req.Protocol)
	}
	m.Host = strings.TrimSpace(req.Host)
	if m.Host == "" {
		return nil, errors.New("host is required")
	}
	m.Port = req.Port
	m.User = strings.TrimSpace(req.User)
	m.Notes = strings.TrimSpace(req.Notes)
	if err := validateMachine(&m); err != nil {
		return nil, err
	}
	// Only a secret that was actually sent can be judged here: an update
	// with no secret object means "keep the current credentials", and
	// what those are is the store's business (see Create/Update).
	if req.Secret != nil {
		if err := checkCredential(m.Protocol, m.AuthType, req.Secret); err != nil {
			return nil, err
		}
	}
	if req.HostKey != "" {
		fingerprint, err := NormalizeFingerprint(req.HostKey)
		if err != nil {
			return nil, err
		}
		m.HostKey = fingerprint
	}
	m.UpdatedAt = time.Now().UTC()
	return &m, nil
}

// validateMachine enforces the per-protocol field rules (port range vs
// baud rate, auth type vs protocol, serial device path shape).
func validateMachine(m *Machine) error {
	switch m.Protocol {
	case ProtocolSSH, ProtocolTelnet, ProtocolMosh:
		if m.Port < 1 || m.Port > 65535 {
			return fmt.Errorf("port must be between 1 and 65535 (got %d)", m.Port)
		}
		switch m.AuthType {
		case AuthPassword, AuthKey, AuthNone:
		default:
			return fmt.Errorf("unknown auth type %q", m.AuthType)
		}
		if m.Protocol == ProtocolSSH && m.AuthType == AuthNone {
			return errors.New("ssh machines need password or key authentication")
		}
		if m.Protocol == ProtocolSSH && m.User == "" {
			return errors.New("ssh machines need a login user")
		}
	case ProtocolSerial:
		m.AuthType = AuthNone
		if !isValidSerialDevice(m.Host) {
			return fmt.Errorf("%q does not look like a serial device path (e.g. /dev/ttyUSB0)", m.Host)
		}
		if !isKnownBaudRate(m.Port) {
			return fmt.Errorf("unsupported baud rate %d (use one of 1200…115200)", m.Port)
		}
	}
	return nil
}

// normalizeProtocolAuth applies the same shape rules the record stores
// with: lower-case, trimmed, and an omitted auth type meaning none.
func normalizeProtocolAuth(protocol, authType string) (string, string) {
	p := strings.ToLower(strings.TrimSpace(protocol))
	a := strings.ToLower(strings.TrimSpace(authType))
	if a == "" {
		a = AuthNone
	}
	return p, a
}

// wantsCredential reports whether this protocol/auth pair cannot work
// without a stored secret. Serial has no authentication at all, and a
// telnet login with no password is a legitimate choice (the operator
// types it at the prompt).
func wantsCredential(protocol, authType string) bool {
	switch authType {
	case AuthKey:
		return protocol == ProtocolSSH || protocol == ProtocolMosh
	case AuthPassword:
		return protocol == ProtocolSSH || protocol == ProtocolMosh || protocol == ProtocolTelnet
	}
	return false
}

// checkCredential refuses a record whose chosen auth type has no
// credential behind it. A machine that can never authenticate is worse
// than a rejected form: it lands in the sidebar and then fails every
// connect with an error pointing at the remote host, never at the field
// that was left blank.
func checkCredential(protocol, authType string, sec *SecretInput) error {
	if !wantsCredential(protocol, authType) {
		return nil
	}
	field, value := "password", ""
	if authType == AuthKey {
		field = "private key"
	}
	if sec != nil {
		value = sec.Password
		if field == "private key" {
			value = sec.SSHKey
		}
	}
	if value != "" {
		return nil
	}
	return fmt.Errorf("%s is required for a %s machine with %s authentication", field, protocol, authType)
}

func isValidSerialDevice(host string) bool {
	if strings.HasPrefix(host, "/") {
		return !strings.Contains(host, "..")
	}
	// Windows-style COM1..COM99.
	if len(host) >= 3 && host[0] == 'C' && host[1] == 'O' && host[2] == 'M' {
		for _, c := range host[3:] {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	return false
}

var knownBaudRates = map[int]bool{
	75: true, 110: true, 134: true, 150: true, 300: true, 600: true,
	1200: true, 2400: true, 4800: true, 9600: true, 19200: true,
	38400: true, 57600: true, 115200: true,
}

func isKnownBaudRate(rate int) bool { return knownBaudRates[rate] }

// NormalizeFingerprint accepts the shapes an operator may paste
// ("ssh-ed25519 SHA256:…", "SHA256:…", "SHA256: …" with a trailing
// comment line from ssh-keyscan) and returns the canonical
// "ssh-<type> SHA256:…" form, or an error when nothing usable is in it.
func NormalizeFingerprint(raw string) (string, error) {
	var keyType, fp string
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		for i, tok := range fields {
			if strings.HasPrefix(tok, "SHA256:") {
				fp = tok
				if i > 0 && strings.HasPrefix(fields[0], "ssh-") {
					keyType = fields[0]
				}
				break
			}
		}
		if fp != "" {
			break
		}
	}
	if fp == "" {
		return "", errors.New("no SHA256: host key fingerprint found")
	}
	if keyType == "" {
		// The bare form is accepted but we can only store what we were
		// given; dials match on the SHA256: part, so this still works.
		return fp, nil
	}
	return keyType + " " + fp, nil
}

// FingerprintPart extracts the bare "SHA256:…" part from a stored pin
// (which may carry the "ssh-… " type prefix).
func FingerprintPart(stored string) string {
	for _, tok := range strings.Fields(stored) {
		if strings.HasPrefix(tok, "SHA256:") {
			return tok
		}
	}
	return stored
}

// ErrNotFound is the sentinel for missing machines.
var ErrNotFound = errors.New("machine not found")

// ErrBuiltinMachine is the sentinel for the built-in host card: it is
// the one record that can exist in the store (UpdateHost persists it)
// and may never be deleted, so Delete refuses the id instead of
// silently removing it. The API layer maps the refusal to the same
// answer it gives for every built-in id.
var ErrBuiltinMachine = errors.New("built-in machine cannot be deleted")

// Store is the on-disk JSON store for machines plus the companion
// secrets file. All mutating methods hold the mutex across the whole
// read-modify-write so concurrent saves cannot interleave.
type Store struct {
	mu sync.Mutex
	// secretMu serialises the read-modify-write of the companion
	// credentials file. See the lock-discipline note above Create.
	secretMu    sync.Mutex
	path        string
	secretsPath string
	machines    map[string]*Machine
	// loadErr is set when either file existed but could not be read at
	// startup. While it is set every save path refuses to run, because
	// the in-memory map is incomplete and a save would overwrite (and
	// thus destroy) the unparseable — but still present — file.
	loadErr error
}

func NewStore(path, secretsPath string) *Store {
	s := &Store{
		path:        path,
		secretsPath: secretsPath,
		machines:    map[string]*Machine{},
	}
	if err := s.load(); err != nil {
		// A corrupt machines file must not take the whole backend down,
		// but it must also never be silently overwritten: keep the
		// error, surface it via saveSecrets/saveLocked (fail closed) and
		// let every read-only path keep working with what parsed.
		s.loadErr = fmt.Errorf("load machines file: %w", err)
		slog.Error("ssh_machines_store_load_failed", "path", path, "err", err)
	} else if err := checkSecretsReadable(secretsPath); err != nil {
		s.loadErr = fmt.Errorf("load secrets file: %w", err)
		slog.Error("ssh_machines_secrets_load_failed", "path", secretsPath, "err", err)
	}
	return s
}

// checkSecretsReadable validates that the credentials file, if present,
// parses — so a later save never overwrites a file we failed to load.
func checkSecretsReadable(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var m secretMap
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var list []*Machine
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.machines = map[string]*Machine{}
	for _, m := range list {
		if m == nil || m.ID == "" {
			continue
		}
		s.machines[m.ID] = m
	}
	return nil
}

func (s *Store) saveLocked() error {
	if s.loadErr != nil {
		// Fail closed: the in-memory set is incomplete; saving would
		// overwrite records we could not read.
		return s.loadErr
	}
	list := make([]*Machine, 0, len(s.machines))
	for _, m := range s.machines {
		// The display flags are derived per read from the secrets file;
		// zero them so the record file never carries a stale true.
		cp := *m
		cp.HasPassword, cp.HasKey, cp.KeyFingerprint = false, false, ""
		// Exactly one record may persist as built-in: the host card's
		// saved account (UpdateHost). User-created machines must never
		// claim the flag.
		cp.Builtin = m.ID == HostMachineID
		list = append(list, &cp)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].Name < list[j].Name
		}
		return list[i].CreatedAt.Before(list[j].CreatedAt)
	})
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.path, data, 0o644)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// secretMap is the on-disk shape of the 0600 credentials file.
type secretMap map[string]MachineSecret

func (s *Store) readSecrets() (secretMap, error) {
	m := secretMap{}
	data, err := os.ReadFile(s.secretsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.secretsPath, err)
	}
	return m, nil
}

func (s *Store) saveSecrets(m secretMap) error {
	// 0600: the file holds live passwords and private keys.
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.secretsPath, data, 0o600)
}

// List returns every machine (secrets never included), sorted by
// creation time. The has_password / has_key / key_fingerprint flags are
// populated from the secrets file for display purposes.
//
// Each record is a COPY, taken under the lock: the display flags are
// written after the lock is released, so handing out the stored pointers
// would be a data race between any two concurrent readers (the transfer
// workers dial while the API serves a list) and would let a caller
// mutate the store's own records by accident.
func (s *Store) List() []*Machine {
	s.mu.Lock()
	out := make([]*Machine, 0, len(s.machines))
	for _, m := range s.machines {
		if m.ID == HostMachineID {
			// The host card has its own serving path (HostMachine,
			// admin-only at the API layer); a raw store listing would
			// hand it to every reader.
			continue
		}
		cp := *m
		out = append(out, &cp)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].Name < out[j].Name
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	secrets, err := s.readSecrets()
	if err == nil {
		for _, m := range out {
			if sec, ok := secrets[m.ID]; ok {
				m.HasPassword = sec.Password != ""
				m.HasKey = sec.SSHKey != ""
			}
			if m.HasKey {
				if fp, fpErr := PublicFingerprint(secrets[m.ID]); fpErr == nil {
					m.KeyFingerprint = fp
				}
			}
		}
	}
	return out
}

// Get returns a copy of one machine (display flags included).
func (s *Store) Get(id string) (*Machine, bool) {
	list := s.List()
	for _, m := range list {
		if m.ID == id {
			return m, true
		}
	}
	return nil, false
}

// getOrErr is Get with an error second value, for the store's own
// mutating methods that return (*Machine, error).
func (s *Store) getOrErr(id string) (*Machine, error) {
	m, ok := s.Get(id)
	if !ok {
		return nil, ErrNotFound
	}
	return m, nil
}

// Lock discipline: s.mu guards the in-memory record map and the
// records-file save (saveLocked, which requires the lock). The secrets
// companion file is I/O'd WITHOUT s.mu — it is a separate file with its
// own atomic write, and holding a mutex across disk writes would
// serialize every list/save behind one slow I/O — but every read-
// modify-write of it runs under secretMu, which is what keeps two
// concurrent upserts from clobbering each other.
func (s *Store) Create(req CreateRequest) (*Machine, error) {
	s.mu.Lock()
	// Name uniqueness: machines are referenced by name in the sidebar
	// the way VMs are, so a duplicate would be a footgun. The host card
	// is not a name the operator chose, so it does not claim one.
	for _, m := range s.machines {
		if m.ID == HostMachineID {
			continue
		}
		if m.Name == strings.TrimSpace(req.Name) {
			s.mu.Unlock()
			return nil, fmt.Errorf("a machine named %q already exists", strings.TrimSpace(req.Name))
		}
	}
	now := time.Now().UTC()
	base := &Machine{
		ID:        uuid.NewString(),
		CreatedAt: now,
		UpdatedAt: now,
	}
	m, err := req.toMachine(base)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	// On a create there is nothing to keep, so an absent secret object IS
	// the missing credential.
	if err := checkCredential(m.Protocol, m.AuthType, req.Secret); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.machines[m.ID] = m
	if err := s.saveLocked(); err != nil {
		delete(s.machines, m.ID)
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()

	if sec := req.Secret.toSecret(); sec != (MachineSecret{}) {
		if err := s.replaceSecret(m.ID, sec); err != nil {
			// Roll the record back: a machine whose credentials
			// could not be stored is a half-created record the
			// operator did not ask for.
			s.rollbackMachine(m.ID)
			return nil, fmt.Errorf("save secrets: %w", err)
		}
	}
	return s.getOrErr(m.ID)
}

// Update modifies an existing machine's record; req.Secret nil leaves the
// credentials untouched, non-nil replaces them wholesale (clear = empty
// fields). If the secrets write fails, the (already valid) record update
// stands and the error is reported — rolling a rename back would lose
// valid user input over an unrelated I/O failure.
func (s *Store) Update(id string, req CreateRequest) (*Machine, error) {
	// An update without a secret object keeps the credential already on
	// disk, and the new auth type still has to be satisfiable by it. The
	// stored one is read before taking s.mu (the secrets file is never
	// touched under it) and only when the requested pair needs a
	// credential at all, so renaming a machine does not suddenly depend
	// on the credentials file being readable.
	if req.Secret == nil {
		if p, a := normalizeProtocolAuth(req.Protocol, req.AuthType); wantsCredential(p, a) {
			sm, err := s.readSecrets()
			if err != nil {
				return nil, fmt.Errorf("check stored credentials: %w", err)
			}
			kept := sm[id]
			if err := checkCredential(p, a, &SecretInput{Password: kept.Password, SSHKey: kept.SSHKey}); err != nil {
				return nil, err
			}
		}
	}
	s.mu.Lock()
	existing, ok := s.machines[id]
	if !ok {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	m, err := req.toMachine(existing)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	m.ID = existing.ID
	m.CreatedAt = existing.CreatedAt
	if req.HostKey == "" {
		m.HostKey = existing.HostKey
	}
	// Renaming into an existing name is refused. The host card is not a
	// name the operator chose, so it does not block a rename.
	for _, other := range s.machines {
		if other.ID != id && other.ID != HostMachineID && other.Name == m.Name {
			s.mu.Unlock()
			return nil, fmt.Errorf("a machine named %q already exists", m.Name)
		}
	}
	s.machines[id] = m
	if err := s.saveLocked(); err != nil {
		s.machines[id] = existing
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()

	if req.Secret != nil {
		if err := s.replaceSecret(id, req.Secret.toSecret()); err != nil {
			return nil, fmt.Errorf("save secrets: %w", err)
		}
	}
	return s.getOrErr(id)
}

// replaceSecret writes one machine's credentials into the companion
// 0600 file. Caller does NOT hold s.mu.
//
// The whole map is read and rewritten for a single id, so the two steps
// must not interleave with another write: without secretMu, two
// concurrent upserts (or an upsert racing a delete) each load the file
// before storing, and the later save silently resurrects or drops the
// other machine's credential. secretMu is what makes that sequence one
// transaction. It is always taken after s.mu, never the other way round.
func (s *Store) replaceSecret(id string, sec MachineSecret) error {
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	sm, err := s.readSecrets()
	if err != nil {
		return err
	}
	sm[id] = sec
	return s.saveSecrets(sm)
}

// dropSecret removes one machine's credentials. Same transaction rules
// as replaceSecret, and the caller must not hold s.mu… except that
// Delete does hold it, which is safe because nothing here takes s.mu.
func (s *Store) dropSecret(id string) error {
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	sm, err := s.readSecrets()
	if err != nil {
		return err
	}
	if _, ok := sm[id]; !ok {
		return nil
	}
	delete(sm, id)
	return s.saveSecrets(sm)
}

// rollbackMachine removes a record again (used when the secrets write
// after a Create failed). Best effort; the failure is logged, not
// propagated — the caller already has the root-cause error.
func (s *Store) rollbackMachine(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.machines[id]; !ok {
		return
	}
	delete(s.machines, id)
	if err := s.saveLocked(); err != nil {
		slog.Error("ssh_machines_rollback_save_failed", "machine", id, "err", err)
	}
}

// Delete removes the machine record, its credentials, and (best effort)
// any runtime state the API layer holds for it. The built-in host card
// is the one record that may never be removed (its id is also reserved
// in Create): without the guard any internal caller — CLI, migration
// script — could delete a persisted card the API layer still serves.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == HostMachineID {
		return ErrBuiltinMachine
	}
	if _, ok := s.machines[id]; !ok {
		return ErrNotFound
	}
	delete(s.machines, id)
	if err := s.saveLocked(); err != nil {
		return err
	}
	return s.dropSecret(id)
}

// GetSecret returns the stored credential for a machine.
//
// The error is separate from the found flag on purpose: an unreadable
// or corrupt credentials file is not "this machine has no password", and
// answering with the empty secret would turn a transient I/O fault into
// a connection that fails with a confusing authentication error.
func (s *Store) GetSecret(id string) (MachineSecret, bool, error) {
	s.mu.Lock()
	_, ok := s.machines[id]
	s.mu.Unlock()
	if !ok {
		return MachineSecret{}, false, nil
	}
	sm, err := s.readSecrets()
	if err != nil {
		return MachineSecret{}, false, err
	}
	sec, ok := sm[id]
	return sec, ok, nil
}

// SetHostKey pins (or re-pins) the host-key fingerprint for a machine.
// The value is normalized; a bare "SHA256:…" pin keeps the stored type
// prefix when one is already there.
func (s *Store) SetHostKey(id string, fingerprint string) (*Machine, error) {
	canon, err := NormalizeFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	m, ok := s.machines[id]
	if !ok {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	// Re-pinning the same key: keep the full form we already have.
	if FingerprintPart(canon) != FingerprintPart(m.HostKey) {
		m.HostKey = canon
		m.UpdatedAt = time.Now().UTC()
		if err := s.saveLocked(); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	s.mu.Unlock()
	return s.getOrErr(id)
}
