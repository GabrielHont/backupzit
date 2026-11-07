package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/repo"
)

func TestExtendedJobOptions(t *testing.T) {
	a := Agent{OS: "Windows 11", Version: "0.47.3", Capabilities: []string{api.CapabilityJobOptions}}
	j := Job{Kind: JobImage, Options: JobOptions{VSS: new(false), ImageDisks: []int{0, 2}}, Retention: repo.RetentionPolicy{KeepYearly: 5}}
	if err := checkExtendedJobOptions(a, j); err != nil {
		t.Fatal(err)
	}
	a.Capabilities = nil
	if err := checkExtendedJobOptions(a, j); err == nil {
		t.Fatal("old agent accepted extended options")
	}
	a.Capabilities = []string{api.CapabilityJobOptions}
	a.OS = "Linux"
	if err := checkExtendedJobOptions(a, j); err == nil {
		t.Fatal("Linux image accepted")
	}
	a.OS = "Windows 11"
	for _, sel := range [][]int{{0, 0}, {-1}} {
		j.Options.ImageDisks = sel
		if err := checkExtendedJobOptions(a, j); err == nil {
			t.Errorf("accepted disks %v", sel)
		}
	}
	var ar api.Run
	o := JobOptions{VSS: new(false), ImageDisks: []int{2, 0}}
	o.apply(&ar, api.KindImageBackup)
	if ar.VSS == nil || *ar.VSS || len(ar.ImageDisks) != 2 {
		t.Fatalf("wire options: %+v", ar)
	}
	if !shorterRetention(repo.RetentionPolicy{KeepYearly: 5}, repo.RetentionPolicy{KeepYearly: 4}) {
		t.Fatal("yearly retention bypassed approval")
	}
}

func TestJobMutationMergeAndValidation(t *testing.T) {
	old := Job{ID: 9, Kind: JobFiles, AgentID: 1, TargetID: 2, Name: "old", Paths: []string{"/data"}, Enabled: true, Options: JobOptions{LimitMBps: 10, PreCommand: "echo before"}, Retention: repo.RetentionPolicy{KeepDaily: 7, KeepYearly: 5}}
	request := func(body string, old Job) (Job, error) {
		r := httptest.NewRequest("PATCH", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		return readJobMutation(r, old, false)
	}
	j, err := request(`{"name":"new","options":{"limit_mbps":0},"retention":{"keep_daily":10}}`, old)
	if err != nil || j.Name != "new" || j.Options.LimitMBps != 0 || j.Options.PreCommand != "echo before" || j.Retention.KeepYearly != 5 {
		t.Fatalf("merge %+v %v", j, err)
	}
	for _, body := range []string{`{"target_id":3}`, `{"enabled":false}`, `{"kind":"vm"}`, `{"options":{"nas_password":"secret"}}`, `{"options":null}`, `{"retention":{"keep_daily":null}}`, `{"schedule":null}`, `{"excludes":[],"unknown":1}`} {
		if _, err := request(body, old); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	old.Kind = JobImage
	old.ImageDisk = new(0)
	if _, err := request(`{"image_disk":1}`, old); err == nil {
		t.Fatal("changed immutable disk")
	}
	if *old.ImageDisk != 0 {
		t.Fatal("decoder mutated the original job")
	}
	b, _ := json.Marshal(toAPIJob(Job{Options: JobOptions{NASPassword: "nas-secret", M365Secret: "app-secret"}}))
	if strings.Contains(string(b), "secret") {
		t.Fatal("public job exposes credentials")
	}
	if err := validateRetention(repo.RetentionPolicy{KeepYearly: -1}); err == nil {
		t.Fatal("negative retention")
	}
}
