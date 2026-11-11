package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/imaging"
)

// Unirea portal: files inside a Windows disk image, through the API. The
// console page already browses an image and restores files from it onto any
// agent (browse.go); these endpoints expose the same, so a firm can bring its
// files back onto a new computer without the console.

type apiImagePartition struct {
	// Ref addresses the partition in /image/files and /image/restore
	// (imaging.PartRef: disk index * 1000 + partition number).
	Ref         int      `json:"ref"`
	Number      int      `json:"number"`
	Size        uint64   `json:"size"`
	MountPoints []string `json:"mount_points,omitempty"`
	Label       string   `json:"label,omitempty"`
	FileSystem  string   `json:"file_system,omitempty"`
	Included    bool     `json:"included"`
	// Browsable: included and NTFS, so its files can be listed and restored.
	Browsable bool `json:"browsable"`
}

type apiImageDisk struct {
	Index      int                 `json:"index"`
	Number     int                 `json:"number"`
	Model      string              `json:"model,omitempty"`
	Size       uint64              `json:"size"`
	Style      string              `json:"style"`
	Partitions []apiImagePartition `json:"partitions"`
}

type apiImageEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

// imageRun loads a run that holds a disk image.
func (s *Server) imageRun(r *http.Request) (Run, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return Run{}, err
	}
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		return Run{}, err
	}
	if run.Kind != api.KindImageBackup || run.SnapshotID == "" {
		return Run{}, apiErr(409, "run %d holds no disk image", id)
	}
	return run, nil
}

// browsablePart checks that ref names an included NTFS partition of the run.
func browsablePart(run Run, ref int) error {
	img, num := imaging.SplitPartRef(ref)
	for _, d := range imageDetails(run) {
		if d.Index != img {
			continue
		}
		for _, p := range d.Partitions {
			if p.Number == num && p.Included && p.FileSystem == "NTFS" {
				return nil
			}
		}
	}
	return apiErr(400, "partition %d holds no files that can be browsed", ref)
}

// imagePath turns a portal path ("/Users/ana") into the image's ("\Users\ana"),
// refusing parent traversal and NUL.
func imagePath(p string) (string, error) {
	if len(p) > 32768 || strings.ContainsRune(p, 0) {
		return "", apiErr(400, "invalid path")
	}
	for _, c := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if c == ".." {
			return "", apiErr(400, "parent traversal is not allowed in image paths")
		}
	}
	return imaging.CleanPath(p), nil
}

func (s *Server) registerAPIImage(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/runs/{id}/image", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		run, err := s.imageRun(r)
		if err != nil {
			return nil, err
		}
		out := []apiImageDisk{}
		for _, d := range imageDetails(run) {
			disk := apiImageDisk{Index: d.Index, Number: d.Number, Model: d.Model, Size: d.Size, Style: d.Style, Partitions: []apiImagePartition{}}
			for _, p := range d.Partitions {
				disk.Partitions = append(disk.Partitions, apiImagePartition{
					Ref: imaging.PartRef(d.Index, p.Number), Number: p.Number, Size: p.Length, MountPoints: p.MountPoints,
					Label: p.Label, FileSystem: p.FileSystem, Included: p.Included, Browsable: p.Included && p.FileSystem == "NTFS",
				})
			}
			out = append(out, disk)
		}
		return out, nil
	}))
	mux.HandleFunc("GET /api/v1/runs/{id}/image/files", s.api(PermRestore, func(w http.ResponseWriter, r *http.Request) (any, error) {
		run, err := s.imageRun(r)
		if err != nil {
			return nil, err
		}
		ref, err := strconv.Atoi(r.URL.Query().Get("part"))
		if err != nil {
			return nil, apiErr(400, "part is required (a ref from /image)")
		}
		if err := browsablePart(run, ref); err != nil {
			return nil, err
		}
		dir, err := imagePath(r.URL.Query().Get("path"))
		if err != nil {
			return nil, err
		}
		v, _, err := s.imageVolume(r.Context(), run, ref)
		if err != nil {
			return nil, apiErr(409, "%s", err)
		}
		list, err := v.List(dir)
		if err != nil {
			return nil, apiErr(404, "%s", err)
		}
		out := make([]apiImageEntry, 0, len(list))
		for _, e := range list {
			kind := "file"
			if e.IsDir {
				kind = "dir"
			}
			out = append(out, apiImageEntry{Name: e.Name, Path: strings.ReplaceAll(e.Path, `\`, "/"), Type: kind, Size: e.Size, ModTime: e.ModTime})
		}
		return out, nil
	}))
	mux.HandleFunc("POST /api/v1/runs/{id}/image/restore", s.api(PermRestore, func(w http.ResponseWriter, r *http.Request) (any, error) {
		run, err := s.imageRun(r)
		if err != nil {
			return nil, err
		}
		var req struct {
			Part    int             `json:"part"`
			Paths   []string        `json:"paths"`
			AgentID int64           `json:"agent_id"`
			Target  json.RawMessage `json:"target"`
		}
		if err := readAPIJSON(r, &req); err != nil {
			return nil, err
		}
		if err := browsablePart(run, req.Part); err != nil {
			return nil, err
		}
		if len(req.Paths) == 0 || len(req.Paths) > 10000 {
			return nil, apiErr(400, "choose between 1 and 10000 paths (use / for the whole partition)")
		}
		paths := make([]string, 0, len(req.Paths))
		for _, p := range req.Paths {
			clean, err := imagePath(p)
			if err != nil {
				return nil, err
			}
			paths = append(paths, clean)
		}
		target, err := restoreDirectory(req.Target)
		if err != nil {
			return nil, err
		}
		if req.AgentID == 0 {
			req.AgentID = run.AgentID
		}
		// The files land on a Windows computer that is still part of the firm.
		a, err := s.store.GetAgent(r.Context(), req.AgentID)
		if err != nil {
			return nil, apiErr(400, "unknown agent")
		}
		if a.RemovedAt != nil || a.Recovery || !strings.Contains(strings.ToLower(a.OS), "windows") {
			return nil, apiErr(400, "files from a disk image can be restored only onto a Windows computer of this console")
		}
		rid, err := s.store.QueueImageFileRestore(r.Context(), run.ID, req.AgentID, req.Part, paths, target)
		if err != nil {
			return nil, mutationAPIError(err, true)
		}
		s.audit(r, "restore.files", "run #%d from image #%d, %d paths to %q on agent %d", rid, run.ID, len(paths), target, req.AgentID)
		queued, err := s.store.GetRun(r.Context(), rid)
		if err != nil {
			return nil, err
		}
		return apiResponse{202, toAPIRunJSON(queued)}, nil
	}))
	// A whole disk of the image back onto a computer started from the
	// recovery medium. Only onto a recovery agent (never a running Windows),
	// only onto a disk of its inventory that is large enough, and only with
	// the explicit confirmation that the disk is erased.
	mux.HandleFunc("POST /api/v1/runs/{id}/image/restore-disk", s.api(PermRestore, func(w http.ResponseWriter, r *http.Request) (any, error) {
		run, err := s.imageRun(r)
		if err != nil {
			return nil, err
		}
		var req struct {
			AgentID      int64 `json:"agent_id"`
			Image        int   `json:"image"`
			TargetDisk   *int  `json:"target_disk"`
			NewHardware  bool  `json:"new_hardware"`
			ConfirmErase bool  `json:"confirm_erase"`
		}
		if err := readAPIJSON(r, &req); err != nil {
			return nil, err
		}
		if !req.ConfirmErase {
			return nil, apiErr(400, "confirm_erase must be true: the target disk is erased")
		}
		if req.TargetDisk == nil {
			return nil, apiErr(400, "target_disk is required")
		}
		a, err := s.store.GetAgent(r.Context(), req.AgentID)
		if err != nil {
			return nil, apiErr(400, "unknown agent")
		}
		if !a.Recovery || a.RemovedAt != nil {
			return nil, apiErr(400, "a whole disk is restored only onto a computer started from the recovery medium")
		}
		disks := imageDetails(run)
		if req.Image < 0 || req.Image >= len(disks) {
			return nil, apiErr(400, "image must name a disk of the backup")
		}
		var need uint64
		for _, p := range disks[req.Image].Partitions {
			need = max(need, p.Offset+p.Length)
		}
		var target *uint64
		for _, d := range a.Disks() {
			if d.Number == *req.TargetDisk {
				size := d.Size
				target = &size
			}
		}
		if target == nil {
			return nil, apiErr(400, "disk %d is not on %s", *req.TargetDisk, a.Hostname)
		}
		if *target < need {
			return nil, apiErr(400, "disk %d holds %d bytes; the image needs at least %d", *req.TargetDisk, *target, need)
		}
		rid, err := s.store.QueueImageRestore(r.Context(), run.ID, a.ID, ImageRestoreOptions{Image: req.Image, TargetDisk: *req.TargetDisk, NewHardware: req.NewHardware})
		if err != nil {
			return nil, mutationAPIError(err, true)
		}
		s.audit(r, "restore.image", "run #%d from backup #%d to disk %d of %s", rid, run.ID, *req.TargetDisk, a.Hostname)
		queued, err := s.store.GetRun(r.Context(), rid)
		if err != nil {
			return nil, err
		}
		return apiResponse{202, toAPIRunJSON(queued)}, nil
	}))
}
