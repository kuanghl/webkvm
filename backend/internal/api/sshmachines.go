package api

// SSH Machines CRUD + connectivity surface.
//
// Permission model (mirrors the VM groups where it makes sense):
//
//   - list / get: any authenticated user (the records carry no
//     secrets — credentials live in the 0600 companion file);
//   - create / update / delete / test / host-key pin: operator+;
//   - terminal ticket + terminal WS + SFTP + transfers: the "console"
//     capability (same seam as the VM console group, so a viewer can
//     be granted machine terminals without being an operator);
//   - the built-in host card is not a record until the admin saves an
//     account for it (UpdateHost), and never leaves the admin's list.
//     Its files are this machine's disk, read and written with webkvm's
//     own privileges, so the file plane answers it for admins only
//     (gateHostCard) while the routes that would have to dial it answer
//     404 to non-admins and 400 to admins (rejectBuiltin): there is
//     nothing to dial.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"webkvm/internal/audit"
	"webkvm/internal/auth"
	"webkvm/internal/models"
	"webkvm/internal/sshmachines"
)

// rejectBuiltin guards the dialing surfaces — terminal ticket, terminal
// WS, host-key pin, test, zmodem — that the built-in host card cannot
// serve, because there is nothing to dial: its "terminal" is
// /api/host/terminal, and its files are served by gateHostCard's
// in-process session.
//
// The answer depends on the caller: a non-admin gets the same 404 as an
// unknown id (confirming the built-in record's existence would be a
// disclosure), while an admin gets a 400 explaining that the card is
// read-only there — the admin knows the card exists, and a bare 404 for
// their own request would read as a bug.
// Returns true when the request was answered (caller must return).
func (h *Handler) rejectBuiltin(w http.ResponseWriter, r *http.Request, id string) bool {
	if id != sshmachines.HostMachineID {
		return false
	}
	if requestIsAdmin(r) {
		jsonErr(w, http.StatusBadRequest, "built-in machine is read-only")
		return true
	}
	jsonErr(w, http.StatusNotFound, "machine not found")
	return true
}

// requestIsAdmin reports whether the caller's role is admin.
func requestIsAdmin(r *http.Request) bool {
	_, role, _ := audit.FromRequest(r)
	return role == string(models.RoleAdmin)
}

// gateHostCard is rejectBuiltin's counterpart on the file plane: the
// built-in host card IS browsable there (an in-process SFTP session, see
// sshmachines/local_sftp.go), but reading and writing it runs with
// webkvm's own privileges, so it belongs to admins alone.
//
// A refusal answers 404, the same as any unknown id: a 403 would confirm
// the record exists. Returns true when the request was answered.
func gateHostCard(w http.ResponseWriter, r *http.Request, id string) bool {
	if id != sshmachines.HostMachineID || requestIsAdmin(r) {
		return false
	}
	jsonErr(w, http.StatusNotFound, "machine not found")
	return true
}

// machineWithSecret loads a machine plus its stored credential.
//
// A credentials-file read error is propagated rather than treated as
// "no credential": dialing with an empty secret would fail later with a
// misleading authentication error instead of the real I/O fault.
func (h *Handler) machineWithSecret(id string) (*sshmachines.Machine, sshmachines.MachineSecret, error) {
	machine, ok := h.machines.Get(id)
	if !ok {
		return nil, sshmachines.MachineSecret{}, errors.New("machine not found")
	}
	sec, _, err := h.machines.GetSecret(id)
	if err != nil {
		return nil, sshmachines.MachineSecret{}, err
	}
	return machine, sec, nil
}

func (h *Handler) ListMachines(w http.ResponseWriter, r *http.Request) {
	out := h.machines.List()
	// The built-in host machine (the terminal that used to be
	// #/host-console) is admin-only and lives only in this response;
	// its saved account (UpdateHost) rides along with it.
	if requestIsAdmin(r) {
		out = append([]*sshmachines.Machine{h.machines.HostMachine()}, out...)
	}
	jsonResp(w, http.StatusOK, out)
}

func (h *Handler) GetMachine(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == sshmachines.HostMachineID {
		if !requestIsAdmin(r) {
			// Same 404 as any missing id: do not disclose the
			// built-in record's existence to non-admins.
			jsonErr(w, http.StatusNotFound, "machine not found")
			return
		}
		jsonResp(w, http.StatusOK, h.machines.HostMachine())
		return
	}
	machine, ok := h.machines.Get(id)
	if !ok {
		jsonErr(w, http.StatusNotFound, "machine not found")
		return
	}
	jsonResp(w, http.StatusOK, machine)
}

// decodeMachineRequest maps the JSON body onto a store request. The
// secret is sent as a nested object; its ABSENCE on update means
// "keep the current credentials" (the UI sends it only when the user
// actually typed new values).
type machineRequest struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	AuthType string `json:"auth_type"`
	Notes    string `json:"notes"`
	Secret   *struct {
		Password         string `json:"password"`
		SSHKey           string `json:"ssh_key"`
		SSHKeyPassphrase string `json:"ssh_key_passphrase"`
	} `json:"secret"`
	HostKey string `json:"host_key"`
}

func (req *machineRequest) toStoreRequest() (sshmachines.CreateRequest, error) {
	sec := (*sshmachines.SecretInput)(nil)
	if req.Secret != nil {
		sec = &sshmachines.SecretInput{
			Password:         req.Secret.Password,
			SSHKey:           req.Secret.SSHKey,
			SSHKeyPassphrase: req.Secret.SSHKeyPassphrase,
		}
		// Normalise away an all-empty secret object (the form may
		// submit it with blank fields meaning "unchanged").
		if sec.Password == "" && sec.SSHKey == "" && sec.SSHKeyPassphrase == "" {
			sec = nil
		}
	}
	return sshmachines.CreateRequest{
		Name:     req.Name,
		Protocol: req.Protocol,
		Host:     req.Host,
		Port:     req.Port,
		User:     req.User,
		AuthType: req.AuthType,
		Notes:    req.Notes,
		Secret:   sec,
		HostKey:  req.HostKey,
	}, nil
}

func (h *Handler) CreateMachine(w http.ResponseWriter, r *http.Request) {
	var req machineRequest
	if err := decodeBody(r, &req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	storeReq, err := req.toStoreRequest()
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	machine, err := h.machines.Create(storeReq)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.audit != nil {
		h.audit.Log(auditFor(r, "ssh_machine.create", "machine:"+machine.ID, map[string]interface{}{
			"name": machine.Name, "protocol": machine.Protocol, "host": machine.Host,
		}))
	}
	jsonResp(w, http.StatusCreated, machine)
}

func (h *Handler) UpdateMachine(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == sshmachines.HostMachineID {
		// The one writable surface of the built-in card: its own
		// account and password (admin-only, inside).
		h.UpdateHostMachine(w, r)
		return
	}
	if h.rejectBuiltin(w, r, id) {
		return
	}
	var req machineRequest
	if err := decodeBody(r, &req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	storeReq, err := req.toStoreRequest()
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	machine, err := h.machines.Update(id, storeReq)
	if err != nil {
		if errors.Is(err, sshmachines.ErrNotFound) {
			jsonErr(w, http.StatusNotFound, err.Error())
			return
		}
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.audit != nil {
		h.audit.Log(auditFor(r, "ssh_machine.update", "machine:"+id, map[string]interface{}{
			"name": machine.Name,
		}))
	}
	jsonResp(w, http.StatusOK, machine)
}

func (h *Handler) DeleteMachine(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if h.rejectBuiltin(w, r, id) {
		return
	}
	machine, ok := h.machines.Get(id)
	if !ok {
		jsonErr(w, http.StatusNotFound, "machine not found")
		return
	}
	if err := h.machines.Delete(id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Runtime state goes away AFTER the record. Sweeping first would let
	// a request that had already read the machine register its terminal
	// (or re-pool an SFTP session) inside that gap and outlive the
	// machine; both surfaces re-check the record once they are
	// registered, so with this order one side always sees the other.
	h.sftpSessions.Close(id)
	if old, loaded := machineTerminalSessions.LoadAndDelete(id); loaded {
		_ = old.(*websocket.Conn).Close()
	}
	if h.audit != nil {
		h.audit.Log(auditFor(r, "ssh_machine.delete", "machine:"+id, map[string]interface{}{
			"name": machine.Name,
		}))
	}
	w.WriteHeader(http.StatusNoContent)
}

// testResult is the shared response shape of POST /{id}/test.
type testResult struct {
	OK          bool   `json:"ok"`
	Status      string `json:"status"`                // connected | hostkey_untrusted | hostkey_changed | unavailable | error
	Fingerprint string `json:"fingerprint,omitempty"` // presented key (untrusted/changed)
	Pinned      string `json:"pinned,omitempty"`      // current pin (changed)
	Message     string `json:"message,omitempty"`
	LatencyMS   int64  `json:"latency_ms,omitempty"`
}

// MachineTest dials the machine WITHOUT touching the terminal and
// reports what happened. The two host-key states are 200/409 with a
// machine-readable status so the UI can drive the accept / re-pin flow;
// a successful auth is "connected".
func (h *Handler) MachineTest(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == sshmachines.HostMachineID {
		// The host card's "test" verifies the stored account against
		// the host's own /bin/login instead of dialing a remote.
		h.TestHostMachine(w, r)
		return
	}
	if h.rejectBuiltin(w, r, id) {
		return
	}
	machine, secret, err := h.machineWithSecret(id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, err.Error())
		return
	}

	start := time.Now()
	res := testResult{Pinned: machine.HostKey}

	switch machine.Protocol {
	case sshmachines.ProtocolSSH:
		client, derr := sshmachines.Dial(machine, secret, 15*time.Second)
		if derr != nil {
			var hke *sshmachines.HostKeyError
			if errors.As(derr, &hke) {
				res.Fingerprint = hke.PresentedFull
				if errors.Is(hke.Err, sshmachines.ErrHostKeyNotPinned) {
					res.OK = false
					res.Status = "hostkey_untrusted"
					res.Message = "This machine has not been trusted yet. Review the presented host key below and pin it to enable the terminal and file browser."
					jsonResp(w, http.StatusOK, res)
					return
				}
				res.OK = false
				res.Status = "hostkey_changed"
				res.Message = "The presented host key no longer matches the pinned fingerprint. Re-pin only if you re-provisioned this host yourself."
				jsonResp(w, http.StatusConflict, res)
				return
			}
			res.OK = false
			res.Status = "error"
			res.Message = derr.Error()
			jsonResp(w, http.StatusBadGateway, res)
			return
		}
		defer client.Close()
		// The dial (and its host-key check) is the meaningful part;
		// an SFTP negotiation would fail for auth-OK-but-no-subsystem
		// servers and muddy the "is it reachable + trusted" answer.
		res.OK = true
		res.Status = "connected"
		res.LatencyMS = time.Since(start).Milliseconds()
		if h.audit != nil {
			h.audit.Log(auditFor(r, "ssh_machine.test", "machine:"+id, map[string]interface{}{
				"result": "connected",
			}))
		}
		jsonResp(w, http.StatusOK, res)

	case sshmachines.ProtocolTelnet:
		conn, err := netDialTCP(machine.Host, machine.Port, 10*time.Second)
		if err != nil {
			res.OK = false
			res.Status = "error"
			res.Message = err.Error()
			jsonResp(w, http.StatusBadGateway, res)
			return
		}
		_ = conn.Close()
		res.OK = true
		res.Status = "connected"
		res.LatencyMS = time.Since(start).Milliseconds()
		jsonResp(w, http.StatusOK, res)

	case sshmachines.ProtocolSerial:
		port, err := serialxProbe(machine.Host, machine.Port)
		if err != nil {
			res.OK = false
			res.Status = "error"
			res.Message = err.Error()
			jsonResp(w, http.StatusBadGateway, res)
			return
		}
		port.Close()
		res.OK = true
		res.Status = "connected"
		jsonResp(w, http.StatusOK, res)

	case sshmachines.ProtocolMosh:
		res.OK = false
		res.Status = "unavailable"
		res.Message = "Mosh is an experimental transport that is not part of this build. Configure the machine for SSH instead."
		jsonResp(w, http.StatusNotImplemented, res)
		return

	default:
		jsonErr(w, http.StatusBadRequest, "unknown protocol")
	}
}

// MachineAcceptHostKey pins (or re-pins) the host-key fingerprint after
// the operator reviewed it in the test dialog.
func (h *Handler) MachineAcceptHostKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if h.rejectBuiltin(w, r, id) {
		return
	}
	var body struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	body.Fingerprint = strings.TrimSpace(body.Fingerprint)
	if body.Fingerprint == "" {
		jsonErr(w, http.StatusBadRequest, "fingerprint is required")
		return
	}
	machine, err := h.machines.SetHostKey(id, body.Fingerprint)
	if err != nil {
		if errors.Is(err, sshmachines.ErrNotFound) {
			jsonErr(w, http.StatusNotFound, err.Error())
			return
		}
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// A key pin is a trust decision: invalidate any pooled SFTP
	// session that was dialed under the previous trust state.
	h.sftpSessions.Close(id)
	if h.audit != nil {
		h.audit.Log(auditFor(r, "ssh_machine.hostkey_pin", "machine:"+id, map[string]interface{}{
			"fingerprint": machine.HostKey,
		}))
	}
	jsonResp(w, http.StatusOK, machine)
}

// MachineTerminalTicket issues a single-use terminal ticket for the
// machine terminal WebSocket (same mechanism as the VM serial
// console). The route sits under requireCapability("console").
func (h *Handler) MachineTerminalTicket(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if h.rejectBuiltin(w, r, id) {
		return
	}
	machine, _, err := h.machineWithSecret(id)
	if err != nil {
		jsonErr(w, http.StatusNotFound, err.Error())
		return
	}
	if machine.Protocol == sshmachines.ProtocolMosh {
		jsonErr(w, http.StatusNotImplemented, "mosh transport is not available in this build")
		return
	}
	user, role, _ := audit.FromRequest(r)
	tk, err := auth.IssueTicket(user, role, auth.TokenEpoch(r))
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "issue ticket: "+err.Error())
		return
	}
	jsonResp(w, http.StatusOK, map[string]any{"ticket": tk, "expires_in": 30})
}
