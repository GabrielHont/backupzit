package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/max-zit/backupzit/internal/api"
)

// vmRestoreMu serialises the checks and the queueing of VM restores from
// the REST API, so two requests cannot both pass the "nothing else is
// restoring this VM" check.
var vmRestoreMu sync.Mutex

// activeRuns returns the kind and the VM restore options of the runs of an
// agent that are queued or running.
func (s *Store) activeRuns(ctx context.Context, agentID int64) ([]Run, error) {
	rows, err := s.db.Query(ctx, `SELECT kind, coalesce(vm_restore, 'null'::jsonb) FROM runs WHERE agent_id=$1 AND status IN ('queued','running','stopping')`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var o []byte
		if err := rows.Scan(&r.Kind, &o); err != nil {
			return nil, err
		}
		r.VMRestore = json.RawMessage(o)
		out = append(out, r)
	}
	return out, rows.Err()
}

// apiVMGuest is a virtual machine held by a VM backup run.
type apiVMGuest struct {
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`
	Platform string `json:"platform"`
	Disks    int    `json:"disks"`
	Size     uint64 `json:"size"`
}

// runGuests lists the guests of a VM backup run (nil for other runs).
func runGuests(r Run) []apiVMGuest {
	d := vmDetails(r)
	if d == nil {
		return nil
	}
	out := make([]apiVMGuest, 0, len(d.Guests))
	for _, g := range d.Guests {
		p := g.Platform
		if p == "" {
			p = "proxmox"
		}
		out = append(out, apiVMGuest{VMID: g.VMID, Name: g.Name, Type: g.Type, Platform: p, Disks: g.Disks, Size: g.Size})
	}
	return out
}

// registerAPIVM: a virtual machine brought back from a VM backup, for the
// Unirea portal (the web UI has its own form).
func (s *Server) registerAPIVM(mux *http.ServeMux) {
	// mode "new" creates a second VM next to the original (Hyper-V: new name
	// and MAC addresses; Proxmox: the next free ID), started only on
	// request. "replace" is refused here: the agent finds the VM to replace
	// by its GUID and, when that is gone, by its name, and the inventory is
	// up to 15 minutes old, so a VM created meanwhile under that name could
	// be overwritten. In place, from the web UI.
	mux.HandleFunc("POST /api/v1/runs/{id}/vm-restore", s.api(PermRestore, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		run, err := s.store.GetRun(r.Context(), id)
		if err != nil {
			return nil, err
		}
		if run.Kind != api.KindVMBackup || run.SnapshotID == "" || vmDetails(run) == nil {
			return nil, apiErr(409, "run %d holds no VM backup", id)
		}
		if run.Status != "success" && run.Status != "warning" {
			return nil, apiErr(409, "run %d did not finish successfully", id)
		}
		var req struct {
			VMID    int    `json:"vmid"`
			Mode    string `json:"mode"`
			Name    string `json:"name"`
			Start   bool   `json:"start"`
			AgentID int64  `json:"agent_id"`
		}
		if err := readAPIJSON(r, &req); err != nil {
			return nil, err
		}
		o := api.VMRestore{VMID: req.VMID, Name: strings.TrimSpace(req.Name), Start: req.Start}
		switch req.Mode {
		case "new":
			o.NewVMID = -1
		case "replace":
			return nil, apiErr(409, "restore this guest in place from the console's web UI")
		default:
			return nil, apiErr(400, `mode is "new" or "replace"`)
		}
		if req.AgentID == 0 {
			req.AgentID = run.AgentID
		}
		a, err := s.store.GetAgent(r.Context(), req.AgentID)
		if err != nil {
			return nil, apiErr(400, "unknown agent")
		}
		if a.RemovedAt != nil || a.Recovery {
			return nil, apiErr(400, "agent %d cannot restore virtual machines", req.AgentID)
		}
		inv := a.PVE()
		if inv == nil {
			return nil, apiErr(409, "%s is not a Proxmox VE node or Hyper-V host", a.Hostname)
		}
		// Queued for a host that is away, the restore would start whenever it
		// comes back, unwatched.
		if !a.Online() {
			return nil, apiErr(409, "%s is not connected now", a.Hostname)
		}
		agentID, cluster := s.clusterRestoreAgent(r.Context(), req.AgentID, o.VMID, o.NewVMID == 0)
		o.HVCluster = cluster
		if o.Name != "" {
			for _, g := range inv.Guests {
				if strings.EqualFold(strings.TrimSpace(g.Name), o.Name) {
					return nil, apiErr(409, "a VM named %q already exists on %s", o.Name, a.Hostname)
				}
			}
		}
		vmRestoreMu.Lock()
		defer vmRestoreMu.Unlock()
		active, err := s.store.activeRuns(r.Context(), agentID)
		if err != nil {
			return nil, err
		}
		for _, x := range active {
			prev := vmRestoreOptions(x)
			if x.Kind != api.KindVMRestore || prev == nil {
				continue
			}
			if prev.VMID == o.VMID {
				return nil, apiErr(409, "VM %d is already being restored on %s", o.VMID, a.Hostname)
			}
			// A failed restore removes the VMs of its name: two restores under
			// one name could take each other's VM down.
			if o.Name != "" && strings.EqualFold(strings.TrimSpace(prev.Name), o.Name) {
				return nil, apiErr(409, "a VM named %q is already being restored on %s", o.Name, a.Hostname)
			}
		}
		rid, err := s.store.QueueVMRestore(r.Context(), run.ID, agentID, o)
		if err != nil {
			return nil, mutationAPIError(err, true)
		}
		s.audit(r, "restore.vm", "run #%d: guest %d from backup #%d (%s)", rid, o.VMID, run.ID, req.Mode)
		queued, err := s.store.GetRun(r.Context(), rid)
		if err != nil {
			return nil, err
		}
		return apiResponse{202, toAPIRunJSON(queued)}, nil
	}))
}
