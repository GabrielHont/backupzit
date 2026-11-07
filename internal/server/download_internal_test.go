package server

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/archiver"
	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/fsutil"
	"github.com/max-zit/backupzit/internal/repo"
)

func TestSnapshotDownloadsWithoutListeners(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "report.txt"), []byte("quarterly report"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	be, err := backend.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be, repo.Password(""))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sn, err := archiver.Run(ctx, r, archiver.Options{Paths: []string{src}})
	if err != nil {
		t.Fatal(err)
	}
	comps, err := fsutil.SnapshotComponents(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := strings.Join(comps, "/")
	file := dir + "/report.txt"
	w := httptest.NewRecorder()
	started, err := streamSnapshotFiles(ctx, w, r, sn, []string{file}, "backup.zip")
	if !started || err != nil || w.Body.String() != "quarterly report" || w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("file: %v %v %q", started, err, w.Body.String())
	}
	for _, p := range []string{dir, ""} {
		w = httptest.NewRecorder()
		started, err = streamSnapshotFiles(ctx, w, r, sn, []string{p}, "backup.zip")
		if !started || err != nil {
			t.Fatalf("zip: %v %v", started, err)
		}
		zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
		if err != nil {
			t.Fatal(err)
		}
		found, empty := false, false
		for _, f := range zr.File {
			if strings.HasSuffix(f.Name, "report.txt") {
				rc, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				b, err := io.ReadAll(rc)
				rc.Close()
				if err != nil || string(b) != "quarterly report" {
					t.Fatalf("archive content: %q %v", b, err)
				}
				found = true
			}
			if strings.HasSuffix(f.Name, "empty/") {
				empty = true
			}
		}
		if !found || !empty {
			t.Fatal("ZIP lost file or empty folder")
		}
	}
	w = httptest.NewRecorder()
	if started, err := streamSnapshotFiles(ctx, w, r, sn, []string{"missing"}, "x.zip"); started || err == nil || w.Body.Len() != 0 {
		t.Fatal("missing path committed a successful download")
	}
}

func TestRecoveryRequestValidation(t *testing.T) {
	for _, p := range []string{"../etc", "/a/../b", "a\x00b", `C:\Data`} {
		if _, err := publicSnapshotPath(p); err == nil {
			t.Errorf("accepted path %q", p)
		}
	}
	for _, raw := range []string{`null`, `"other"`, `{}`, `{"directory":""}`, `{"directory":"C:\\Restore","extra":1}`} {
		if _, err := restoreDirectory([]byte(raw)); err == nil {
			t.Errorf("accepted target %s", raw)
		}
	}
	for _, raw := range []string{`"original"`, `{"directory":"C:\\Restore"}`} {
		if _, err := restoreDirectory([]byte(raw)); err != nil {
			t.Error(err)
		}
	}
}
