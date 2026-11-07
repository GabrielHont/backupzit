package server

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/max-zit/backupzit/internal/repo"
)

// streamSnapshotFiles is shared by the UI and API. It resolves all top-level
// selections before committing headers; later I/O errors abort the transfer.
// Data blobs and ZIP entries are written directly to the response.
func streamSnapshotFiles(ctx context.Context, w http.ResponseWriter, r *repo.Repository, sn *repo.Snapshot, sel []string, zipName string) (bool, error) {
	if len(sel) == 0 {
		return false, fmt.Errorf("choose at least one file or folder")
	}
	nodes := make([]*repo.Node, 0, len(sel))
	for _, p := range sel {
		_, n, err := lookupNode(ctx, r, sn.Tree, snapshotComps(p))
		if err != nil {
			return false, err
		}
		if n == nil {
			n = &repo.Node{Name: "backup", Type: repo.NodeDir, Subtree: &sn.Tree, ModTime: sn.Time}
		}
		nodes = append(nodes, n)
	}
	writeFile := func(dst io.Writer, n *repo.Node) error {
		for _, id := range n.Content {
			if err := ctx.Err(); err != nil {
				return err
			}
			b, err := r.LoadBlob(ctx, repo.DataBlob, id)
			if err != nil {
				return err
			}
			if _, err := dst.Write(b); err != nil {
				return err
			}
		}
		return nil
	}
	if len(sel) == 1 {
		n := nodes[0]
		name := n.Name
		if n.Type == repo.NodeSymlink {
			var err error
			n, err = followLink(ctx, r, sn.Tree, snapshotComps(sel[0]), n)
			if err != nil {
				return false, err
			}
		}
		if n.Type == repo.NodeFile {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
			w.Header().Set("Content-Length", strconv.FormatUint(n.Size, 10))
			w.WriteHeader(http.StatusOK)
			return true, writeFile(w, n)
		}
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": zipName}))
	w.WriteHeader(http.StatusOK)
	zw := zip.NewWriter(w)
	var add func(string, []string, *repo.Node) error
	add = func(rel string, comps []string, n *repo.Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n.Name == "." || n.Name == ".." || strings.ContainsAny(n.Name, "/\\\x00") {
			return fmt.Errorf("invalid snapshot entry name")
		}
		switch n.Type {
		case repo.NodeDir:
			if n.Subtree == nil {
				return fmt.Errorf("folder %s has no tree", rel)
			}
			t, err := r.LoadTree(ctx, *n.Subtree)
			if err != nil {
				return err
			}
			if len(t.Nodes) == 0 {
				_, err := zw.CreateHeader(&zip.FileHeader{Name: rel + "/", Modified: n.ModTime})
				return err
			}
			for i := range t.Nodes {
				if err := add(path.Join(rel, t.Nodes[i].Name), append(append([]string{}, comps...), t.Nodes[i].Name), &t.Nodes[i]); err != nil {
					return err
				}
			}
		case repo.NodeFile:
			f, err := zw.CreateHeader(&zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: n.ModTime})
			if err != nil {
				return err
			}
			return writeFile(f, n)
		case repo.NodeSymlink:
			// As in the UI, links to files become files and links to folders
			// are omitted to avoid loops.
			if target, err := followLink(ctx, r, sn.Tree, comps, n); err == nil && target.Type == repo.NodeFile {
				f, err := zw.CreateHeader(&zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: target.ModTime})
				if err != nil {
					return err
				}
				return writeFile(f, target)
			}
		}
		return nil
	}
	for i, n := range nodes {
		if err := add(n.Name, snapshotComps(sel[i]), n); err != nil {
			return true, err
		}
	}
	return true, zw.Close()
}

func (s *Server) downloadFiles(w http.ResponseWriter, r *http.Request, run Run, sel []string) (bool, error) {
	repository, sn, err := s.fileSnapshot(r.Context(), run)
	if err != nil {
		return false, err
	}
	s.audit(r, "restore.download", "%d paths from backup #%d", len(sel), run.ID)
	return streamSnapshotFiles(r.Context(), w, repository, sn, sel, fmt.Sprintf("backup%d.zip", run.ID))
}
