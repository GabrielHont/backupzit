package server

import (
	"net/http"
	"strings"

	"github.com/max-zit/backupzit/internal/api"
)

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
	// and MAC addresses; Proxmox: the next free ID); "replace" puts the
	// backup in place of the original, which is turned off first (Hyper-V
	// keeps its old disks renamed). The restored VM is started only on
	// request.
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
			o.Overwrite = true
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
		agentID, cluster := s.clusterRestoreAgent(r.Context(), req.AgentID, o.VMID, o.NewVMID == 0)
		o.HVCluster = cluster
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
