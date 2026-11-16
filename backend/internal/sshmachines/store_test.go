package sshmachines

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The store must never overwrite a file it could not parse: an empty
// in-memory map saved over it would destroy every record (and every
// credential) on disk.

func sshReq(name string) CreateRequest {
	return CreateRequest{
		Name:     name,
		Protocol: ProtocolSSH,
		Host:     "10.0.0.1",
		Port:     22,
		User:     "root",
		AuthType: AuthPassword,
		Secret:   &SecretInput{Password: "hunter2"},
	}
}

func TestCreateFailsClosedOnCorruptRecordsFile(t *testing.T) {
	dir := t.TempDir()
	records := filepath.Join(dir, "ssh-machines.json")
	secrets := filepath.Join(dir, "ssh-machine-secrets.json")
	const corrupt = "{ this is not valid json"
	if err := os.WriteFile(records, []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewStore(records, secrets)
	if _, err := s.Create(sshReq("router")); err == nil {
		t.Fatal("Create succeeded while the records file was unreadable")
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("List = %d records, want 0 (nothing may be added while loadErr is set)", len(got))
	}
	data, err := os.ReadFile(records)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != corrupt {
		t.Fatalf("the unparseable file was overwritten:\n%q", data)
	}
	if _, err := os.Stat(secrets); !os.IsNotExist(err) {
		t.Fatalf("secrets file must not be written by a refused create (err=%v)", err)
	}
}

func TestCreateFailsClosedOnCorruptSecretsFile(t *testing.T) {
	dir := t.TempDir()
	records := filepath.Join(dir, "ssh-machines.json")
	secrets := filepath.Join(dir, "ssh-machine-secrets.json")
	if err := os.WriteFile(records, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	const corrupt = "[ credentials, not valid"
	if err := os.WriteFile(secrets, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewStore(records, secrets)
	if _, err := s.Create(sshReq("router")); err == nil {
		t.Fatal("Create succeeded while the secrets file was unreadable")
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("the rolled-back record survived: %d", len(got))
	}
	data, err := os.ReadFile(secrets)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != corrupt {
		t.Fatalf("the unparseable secrets file was overwritten:\n%q", data)
	}
}

func TestHealthyStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	records := filepath.Join(dir, "ssh-machines.json")
	secrets := filepath.Join(dir, "ssh-machine-secrets.json")

	s := NewStore(records, secrets)
	m, err := s.Create(sshReq("router"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Credentials live in their own file, mode 0600.
	fi, err := os.Stat(secrets)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("secrets file mode = %o, want 600", perm)
	}
	recData, err := os.ReadFile(records)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"hunter2", "secret"} {
		if strings.Contains(string(recData), leak) {
			t.Fatalf("the records file leaks credential material (%q)", leak)
		}
	}

	sec, ok, err := s.GetSecret(m.ID)
	if err != nil || !ok || sec.Password != "hunter2" {
		t.Fatalf("GetSecret = %+v ok=%v err=%v", sec, ok, err)
	}

	// Display flags come from the secrets file but are never persisted.
	list := s.List()
	if len(list) != 1 || !list[0].HasPassword {
		t.Fatalf("List = %+v, want one machine with HasPassword", list)
	}
	recData, err = os.ReadFile(records)
	if err != nil {
		t.Fatal(err)
	}
	// saveLocked zeroes the display flags before writing, so the record
	// file can never carry a stale "true" (they do still serialize as
	// false: has_password/HasKey have no omitempty).
	if !strings.Contains(string(recData), `"name": "router"`) ||
		strings.Contains(string(recData), `"has_password": true`) ||
		strings.Contains(string(recData), `"has_key": true`) {
		t.Fatalf("records file should hold the record with the display flags zeroed:\n%s", recData)
	}

	if err := s.Delete(m.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, err := s.GetSecret(m.ID); err != nil || ok {
		t.Fatal("the credential survived the delete")
	}
}

// TestListHandsOutCopiesAndConcurrentReadersDoNotRace pins the fix for
// the display-flag race: List used to collect the store's own *Machine
// pointers, release the lock and then write HasPassword / HasKey /
// KeyFingerprint onto them. Any two concurrent readers — an API list
// next to a transfer worker dialing the same machine — wrote the same
// bytes. Copies also mean a caller cannot write through a read.
func TestListHandsOutCopiesAndConcurrentReadersDoNotRace(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "ssh-machines.json"), filepath.Join(dir, "ssh-machine-secrets.json"))
	alpha, err := s.Create(sshReq("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	beta, err := s.Create(sshReq("beta"))
	if err != nil {
		t.Fatal(err)
	}

	byName := func(list []*Machine, name string) *Machine {
		for _, m := range list {
			if m.Name == name {
				return m
			}
		}
		t.Fatalf("no machine %q in %d records", name, len(list))
		return nil
	}

	list := s.List()
	if !byName(list, "alpha").HasPassword {
		t.Fatal("the display flag is missing even on a plain read")
	}
	got := byName(list, "alpha")
	got.HostKey = "ssh-ed25519 SHA256:scribbled"
	got.Notes = "scribbled"
	got.HasPassword = false
	after := byName(s.List(), "alpha")
	if after.HostKey != "" || after.Notes != "" || !after.HasPassword {
		t.Errorf("writing through a List result changed the store: %+v", after)
	}

	// The flag write happens outside the lock on purpose (reading the
	// secrets file must not serialize behind the record map), so this is
	// the shape that used to trip the race detector.
	ids := []string{alpha.ID, beta.ID}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				switch (i + j) % 3 {
				case 0:
					s.List()
				case 1:
					if _, ok := s.Get(ids[i%len(ids)]); !ok {
						t.Errorf("Get(%s) lost the record", ids[i%len(ids)])
						return
					}
				default:
					if _, ok, err := s.GetSecret(ids[i%len(ids)]); err != nil || !ok {
						t.Errorf("GetSecret(%s) found nothing (err %v)", ids[i%len(ids)], err)
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestConcurrentSecretWritesKeepEveryCredential is the regression for the
// read-modify-write race: the credentials file holds every machine's
// secret in one map, so two writers that both load it before either
// stores clobber each other, and a delete racing an upsert can
// resurrect a credential that was just removed.
func TestConcurrentSecretWritesKeepEveryCredential(t *testing.T) {
	dir := t.TempDir()
	records := filepath.Join(dir, "ssh-machines.json")
	secrets := filepath.Join(dir, "ssh-machine-secrets.json")
	s := NewStore(records, secrets)

	const n = 8
	var wg sync.WaitGroup
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := sshReq(fmt.Sprintf("host-%d", i))
			req.Secret = &SecretInput{Password: fmt.Sprintf("pw-%d", i)}
			m, err := s.Create(req)
			if err != nil {
				t.Errorf("Create host-%d: %v", i, err)
				return
			}
			ids[i] = m.ID
		}(i)
	}
	wg.Wait()

	// Every credential must be on disk, with the password it was given.
	for i, id := range ids {
		if id == "" {
			t.Fatalf("machine %d was never registered", i)
		}
		sec, ok, err := s.GetSecret(id)
		if err != nil || !ok {
			t.Fatalf("GetSecret(%s) = ok %v err %v", id, ok, err)
		}
		if want := fmt.Sprintf("pw-%d", i); sec.Password != want {
			t.Errorf("machine %d password = %q, want %q (a concurrent writer clobbered it)", i, sec.Password, want)
		}
	}

	// And a delete must not lose or restore anyone else's credential.
	victim := ids[0]
	wg = sync.WaitGroup{}
	for i := 1; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := sshReq(fmt.Sprintf("host-%d", i))
			req.Secret = &SecretInput{Password: fmt.Sprintf("rotated-%d", i)}
			if _, err := s.Update(ids[i], req); err != nil {
				t.Errorf("Update %d: %v", i, err)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.Delete(victim); err != nil {
			t.Errorf("Delete: %v", err)
		}
	}()
	wg.Wait()

	if _, ok, err := s.GetSecret(victim); err != nil || ok {
		t.Errorf("deleted credential came back: ok=%v err=%v", ok, err)
	}
	for i := 1; i < n; i++ {
		sec, ok, err := s.GetSecret(ids[i])
		if err != nil || !ok {
			t.Fatalf("GetSecret(%s) after the delete race: ok %v err %v", ids[i], ok, err)
		}
		if want := fmt.Sprintf("rotated-%d", i); sec.Password != want {
			t.Errorf("machine %d password = %q, want %q", i, sec.Password, want)
		}
	}
}

// TestGetSecretReportsReadErrors: an unreadable credentials file is not
// "no password stored". Callers must see the fault, or a disk problem
// turns into a misleading authentication failure.
func TestGetSecretReportsReadErrors(t *testing.T) {
	dir := t.TempDir()
	records := filepath.Join(dir, "ssh-machines.json")
	secrets := filepath.Join(dir, "ssh-machine-secrets.json")
	s := NewStore(records, secrets)
	m, err := s.Create(sshReq("router"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := s.GetSecret(m.ID); err != nil {
		t.Fatalf("GetSecret on a healthy file: %v", err)
	}

	if err := os.WriteFile(secrets, []byte("{ truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetSecret(m.ID); err == nil {
		t.Error("GetSecret must report a corrupt credentials file")
	}
	// Writes stay closed: the unparseable file must not be replaced by
	// whatever the store can still remember.
	if _, err := s.Update(m.ID, sshReq("router")); err == nil {
		t.Error("Update must fail while the credentials file cannot be parsed")
	}
	data, err := os.ReadFile(secrets)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{ truncated" {
		t.Error("the unparseable secrets file was overwritten")
	}
}

// TestCredentialRequiredForTheChosenAuthType: a machine that cannot
// authenticate is worse than a rejected form. It appears in the sidebar
// and every connect then fails with an error pointing at the remote
// host, never at the field that was left blank.
func TestCredentialRequiredForTheChosenAuthType(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "ssh-machines.json"), filepath.Join(dir, "ssh-machine-secrets.json"))

	blank := sshReq("router")
	blank.Secret = &SecretInput{}
	if _, err := s.Create(blank); err == nil || !strings.Contains(err.Error(), "password is required") {
		t.Errorf("Create with a blank password: err = %v, want it to name the missing password", err)
	}

	keyless := sshReq("keyless")
	keyless.AuthType = AuthKey
	keyless.Secret = &SecretInput{Password: "hunter2"}
	if _, err := s.Create(keyless); err == nil || !strings.Contains(err.Error(), "private key is required") {
		t.Errorf("Create with key auth and no key: err = %v", err)
	}

	// The shapes that legitimately need no stored secret.
	noCred := []CreateRequest{
		{Name: "console", Protocol: ProtocolSerial, Host: "/dev/ttyUSB0", Port: 9600},
		{Name: "switch", Protocol: ProtocolTelnet, Host: "10.0.0.2", Port: 23, AuthType: AuthNone},
	}
	for _, req := range noCred {
		if _, err := s.Create(req); err != nil {
			t.Errorf("Create %s: %v", req.Name, err)
		}
	}

	// Switching an existing password machine to key auth without sending
	// a key must be refused: the kept credential is a password.
	m, err := s.Create(sshReq("router"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	toKey := sshReq("router")
	toKey.AuthType = AuthKey
	if _, err := s.Update(m.ID, toKey); err == nil || !strings.Contains(err.Error(), "private key is required") {
		t.Errorf("Update to key auth without a key: err = %v", err)
	}
	// The record is untouched: the refusal must not half-apply.
	if got, ok := s.Get(m.ID); !ok || got.AuthType != AuthPassword {
		t.Fatalf("record changed by the refused update: %+v ok %v", got, ok)
	}
	// With the key supplied it goes through, and a rename that changes
	// nothing about auth still works.
	toKey.Secret = &SecretInput{SSHKey: "ssh-key-material"}
	if got, err := s.Update(m.ID, toKey); err != nil || got.AuthType != AuthKey {
		t.Fatalf("Update with a key: %+v err %v", got, err)
	}
	rename := sshReq("gateway")
	rename.AuthType = AuthKey
	rename.Secret = nil
	if got, err := s.Update(m.ID, rename); err != nil || got.Name != "gateway" {
		t.Fatalf("rename without touching credentials: %+v err %v", got, err)
	}
	// A create with no secret object at all: nothing is being kept, so
	// the credential is simply missing.
	nosecret := sshReq("plain")
	nosecret.Secret = nil
	if _, err := s.Create(nosecret); err == nil || !strings.Contains(err.Error(), "password is required") {
		t.Errorf("Create without any secret object: err = %v", err)
	}
}
