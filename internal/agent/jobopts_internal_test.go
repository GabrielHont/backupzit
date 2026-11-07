package agent

import (
	"github.com/max-zit/backupzit/internal/api"
	"testing"
)

func TestRunVSSOverride(t *testing.T) {
	a := &Agent{VSS: true}
	if !a.runVSS(api.Run{}) {
		t.Fatal("lost agent default")
	}
	if a.runVSS(api.Run{VSS: new(false)}) {
		t.Fatal("ignored false override")
	}
	a.VSS = false
	if !a.runVSS(api.Run{VSS: new(true)}) {
		t.Fatal("ignored true override")
	}
}
