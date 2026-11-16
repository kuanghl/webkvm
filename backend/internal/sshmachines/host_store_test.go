package sshmachines

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The built-in host card is the one record the operator is allowed to
// configure (UpdateHost) but never to create or delete: it must persist
// into the same two files as every other machine, stay out of List(), and
// survive a reload with its credential intact.

func newHostTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return NewStore(filepath.Join(dir, "ssh-machines.json"), filepath.Join(dir, "ssh-machine-secrets.json"))
}

func TestUpdateHostPersistsRecordAndSecret(t *testing.T) {
	s := newHostTestStore(t)
	m, err := s.UpdateHost(UpdateHostRequest{
		User:     "khl",
		AuthType: AuthPassword,
		Notes:    "nightly",
		Secret:   &SecretInput{Password: "hunter2"},
	})
	if err != nil {
		t.Fatalf("UpdateHost: %v", err)
	}
	if m.ID != HostMachineID || m.User != "khl" || m.AuthType != AuthPassword || !m.HasPassword {
		t.Fatalf("UpdateHost = %+v, want the host card with a stored password", m)
	}

	// Record file: the one builtin:true entry, and no credential in it.
	records, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(records), `"builtin":true`) && !strings.Contains(string(records), `"builtin": true`) {
		t.Errorf("records file does not carry builtin:true for the host card: %s", records)
	}
	if strings.Contains(string(records), "hunter2") {
		t.Errorf("records file carries the password: %s", records)
	}

	// Secrets file: 0600, and the password lives under the "host" key.
	st, err := os.Stat(s.secretsPath)
	if err != nil {
		t.Fatalf("secrets file: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("secrets file mode = %o, want 600", got)
	}
	sec, ok, err := s.GetSecret(HostMachineID)
	if err != nil || !ok || sec.Password != "hunter2" {
		t.Fatalf("GetSecret(host) = %q, %v, %v, want the stored password", sec.Password, ok, err)
	}
}

func TestListExcludesHostCard(t *testing.T) {
	s := newHostTestStore(t)
	if _, err := s.UpdateHost(UpdateHostRequest{User: "khl", AuthType: AuthPassword, Secret: &SecretInput{Password: "x"}}); err != nil {
		t.Fatal(err)
	}
	for _, m := range s.List() {
		if m.ID == HostMachineID {
			t.Fatal("List() must not hand the host card to plain readers")
		}
	}
}

func TestUpdateHostKeepPassword(t *testing.T) {
	s := newHostTestStore(t)
	if _, err := s.UpdateHost(UpdateHostRequest{User: "khl", AuthType: AuthPassword, Secret: &SecretInput{Password: "first"}}); err != nil {
		t.Fatal(err)
	}
	// Nil secret in password mode = "keep the stored one".
	if _, err := s.UpdateHost(UpdateHostRequest{User: "khl", AuthType: AuthPassword}); err != nil {
		t.Fatalf("keep-password update: %v", err)
	}
	sec, _, err := s.GetSecret(HostMachineID)
	if err != nil || sec.Password != "first" {
		t.Fatalf("password changed on keep: %q, %v", sec.Password, err)
	}
}

func TestUpdateHostKeepWithoutStoredPasswordErrors(t *testing.T) {
	s := newHostTestStore(t)
	if _, err := s.UpdateHost(UpdateHostRequest{User: "khl", AuthType: AuthPassword}); err == nil {
		t.Fatal("keep with nothing stored must fail: there is no password to keep")
	}
}

func TestUpdateHostNoneClearsSecret(t *testing.T) {
	s := newHostTestStore(t)
	if _, err := s.UpdateHost(UpdateHostRequest{User: "khl", AuthType: AuthPassword, Secret: &SecretInput{Password: "x"}}); err != nil {
		t.Fatal(err)
	}
	m, err := s.UpdateHost(UpdateHostRequest{User: "", AuthType: AuthNone})
	if err != nil {
		t.Fatalf("switch to manual: %v", err)
	}
	if m.HasPassword {
		t.Fatal("manual mode must not claim a stored password")
	}
	sec, ok, err := s.GetSecret(HostMachineID)
	if err != nil || ok || sec.Password != "" {
		t.Fatalf("secret not cleared: %+v, %v, %v", sec, ok, err)
	}
}

func TestUpdateHostRejectsKeyAuth(t *testing.T) {
	s := newHostTestStore(t)
	if _, err := s.UpdateHost(UpdateHostRequest{User: "khl", AuthType: "key", Secret: &SecretInput{SSHKey: "x"}}); err == nil {
		t.Fatal("a getty login has no key half: key auth must be refused")
	}
	if _, err := s.UpdateHost(UpdateHostRequest{User: "khl", AuthType: "mosh"}); err == nil {
		t.Fatal("unknown auth types must be refused")
	}
}

func TestUpdateHostValidation(t *testing.T) {
	s := newHostTestStore(t)
	if _, err := s.UpdateHost(UpdateHostRequest{User: "", AuthType: AuthPassword, Secret: &SecretInput{Password: "x"}}); err == nil {
		t.Error("password mode without a user must fail")
	}
	if _, err := s.UpdateHost(UpdateHostRequest{User: "khl", AuthType: AuthPassword, Secret: &SecretInput{}}); err == nil {
		t.Error("password mode with a blank password must fail")
	}
}

func TestUpdateHostSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	records := filepath.Join(dir, "ssh-machines.json")
	secrets := filepath.Join(dir, "ssh-machine-secrets.json")
	s1 := NewStore(records, secrets)
	if _, err := s1.UpdateHost(UpdateHostRequest{User: "khl", AuthType: AuthPassword, Secret: &SecretInput{Password: "hunter2"}}); err != nil {
		t.Fatal(err)
	}

	s2 := NewStore(records, secrets)
	m := s2.HostMachine()
	if m.User != "khl" || m.AuthType != AuthPassword || !m.HasPassword {
		t.Fatalf("reload lost the saved account: %+v", m)
	}
	sec, _, err := s2.GetSecret(HostMachineID)
	if err != nil || sec.Password != "hunter2" {
		t.Fatalf("reload lost the stored password: %q, %v", sec.Password, err)
	}
}

func TestHostCardDoesNotBlockMachineNames(t *testing.T) {
	s := newHostTestStore(t)
	// A hostname collision is exactly what used to be a footgun: the
	// card is not a name the operator chose, so it must not block one.
	hostName := SyntheticHost().Name
	req := sshReq("router")
	req.Name = hostName
	if _, err := s.Create(req); err != nil {
		t.Fatalf("Create(%q) blocked by the host card: %v", hostName, err)
	}
	if _, err := s.Create(sshReq(hostName)); err == nil {
		t.Fatal("a real duplicate still has to fail")
	}
}

// TestDeleteRefusesHostID pins the store-level guard: the host card is
// the one persisted record that may never be removed, and the refusal
// must hold even before any API layer is involved (the CLI and
// migration scripts call the store directly).
func TestDeleteRefusesHostID(t *testing.T) {
	s := newHostTestStore(t)
	if _, err := s.UpdateHost(UpdateHostRequest{User: "khl", AuthType: AuthPassword, Secret: &SecretInput{Password: "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(HostMachineID); !errors.Is(err, ErrBuiltinMachine) {
		t.Fatalf("Delete(host) = %v, want ErrBuiltinMachine", err)
	}
	// The record's secret survives the attempt.
	if sec, ok, _ := s.GetSecret(HostMachineID); !ok || sec.Password != "x" {
		t.Fatalf("GetSecret(host) after refused delete = %q, %v, want the stored password", sec.Password, ok)
	}
	// An unknown id still gets the ordinary sentinel.
	if err := s.Delete("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(unknown) = %v, want ErrNotFound", err)
	}
}
