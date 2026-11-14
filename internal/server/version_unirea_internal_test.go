package server

import (
	"os"
	"path/filepath"
	"testing"
)

// Unirea fork: the fork's agents and installers compare as 0.47.3NN.
func TestUnireaVersions(t *testing.T) {
	for v, want := range map[string][3]int{
		"0.47.3":                {0, 47, 3},
		"0.47.3-unirea.7":       {0, 47, 307},
		"v0.47.3-unirea.12":     {0, 47, 312},
		"0.47.307":              {0, 47, 307},
		"0.47.3-legacy":         {0, 47, 3},
		"0.47.3-unirea.7-dirty": {0, 47, 307},
	} {
		if got, ok := parseVersion(v); !ok || got != want {
			t.Errorf("%s: %v %v", v, got, ok)
		}
	}
	dist := t.TempDir()
	for _, n := range []string{"backupzit-agent-0.47.3-x64.msi", "backupzit-agent-0.47.307-x64.msi"} {
		if err := os.WriteFile(filepath.Join(dist, n), []byte("msi"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{DistDir: dist}
	up := s.availableUpdates([]Agent{
		{ID: 1, OS: "Windows 11", Arch: "amd64", Version: "0.47.3"},
		{ID: 2, OS: "Windows 11", Arch: "amd64", Version: "0.47.3-unirea.7"},
	})
	if up[1].File != "backupzit-agent-0.47.307-x64.msi" || up[1].Version != "0.47.307" {
		t.Errorf("official agent: %+v", up[1])
	}
	if _, ok := up[2]; ok {
		t.Errorf("fork agent offered its own version again: %+v", up[2])
	}
}
