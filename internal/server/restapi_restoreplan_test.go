package server_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

// Unirea fork: a whole computer restored from the portal, without boot media.
func TestAPIRestorePlans(t *testing.T) {
	e := setup(t)
	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	op := apiToken(t, c, "operator")
	target, err := e.store.CreateTarget(e.ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(host string, recovery bool, token string, caps []string, disks string) (int64, error) {
		if token == "" {
			token, _, _ = e.store.CreateEnrollmentToken(e.ctx, time.Hour)
		}
		resp, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: token, Hostname: host, OS: "windows", Version: "test", Recovery: recovery, Capabilities: caps})
		if err != nil {
			return 0, err
		}
		a, err := e.store.AuthenticateAgent(e.ctx, resp.AgentUUID, resp.Secret)
		if err != nil {
			t.Fatal(err)
		}
		poll := api.PollRequest{Hostname: host, OS: "windows", Version: "test", Capabilities: caps}
		if disks != "" {
			poll.Disks = json.RawMessage(disks)
		}
		if err := e.store.TouchAgent(e.ctx, a.ID, poll, ""); err != nil {
			t.Fatal(err)
		}
		return a.ID, nil
	}
	caps := []string{api.CapabilityJobOptions, server.CapabilityRecoveryPrepare}
	newDisk := `[{"number":0,"model":"Msft Virtual Disk","size":161061273600,"sector_size":512,"style":"gpt","gpt_disk_id":"AAAA-NEW","system":true,"partitions":[{"number":3,"offset":122683392,"length":160000000000,"mount_points":["C:\\"],"file_system":"NTFS"}]}]`
	old, _ := enroll("PC-VECHI", false, "", caps, "")
	fresh, _ := enroll("PC-NOU", false, "", caps, newDisk)
	tooOld, _ := enroll("PC-FARA-ACTUALIZARE", false, "", []string{api.CapabilityJobOptions}, newDisk)

	disk := func(n int) *int { return &n }
	jobID, err := e.store.CreateJob(e.ctx, server.Job{Kind: server.JobImage, AgentID: old, TargetID: target, Name: "Tot calculatorul", ImageDisk: disk(-1)})
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := e.store.QueueBackup(e.ctx, jobID, "manual")
	if cl, err := e.store.ClaimRun(e.ctx, old); err != nil || cl == nil {
		t.Fatalf("claim: %v", err)
	}
	details := `[{"number":0,"model":"Msft Virtual Disk","size":146028888064,"sector_size":512,"style":"gpt","head":"` + strings.Repeat("ab", 32) + `",
	  "partitions":[{"number":1,"offset":1048576,"length":104857600,"file_system":"FAT32","included":true},
	                {"number":3,"offset":122683392,"length":100000000000,"mount_points":["C:\\"],"file_system":"NTFS","included":true}]}]`
	if err := e.store.FinishRun(e.ctx, old, runID, api.RunResult{Status: api.StatusSuccess, SnapshotID: strings.Repeat("cd", 32), Details: json.RawMessage(details)}); err != nil {
		t.Fatal(err)
	}
	create := func(agentID int64, confirm bool) (int, string) {
		return apiJSONCall(t, e, op, "POST", "/api/v1/restore-plans", fmt.Sprintf(`{"run_id":%d,"agent_id":%d,"confirm_erase":%v}`, runID, agentID, confirm))
	}
	type plan struct {
		ID      int64
		State   string
		Writing bool
		Message string
	}
	get := func(id int64) plan {
		_, body := apiJSONCall(t, e, op, "GET", fmt.Sprintf("/api/v1/restore-plans/%d", id), "")
		var p plan
		_ = json.Unmarshal([]byte(body), &p)
		return p
	}
	// Refused: no confirmation; a computer whose program cannot prepare.
	if code, body := create(fresh, false); code != 400 {
		t.Fatalf("no confirmation: %d %s", code, body)
	}
	if code, body := create(tooOld, true); code != 400 || !strings.Contains(body, "too old") {
		t.Fatalf("old agent: %d %s", code, body)
	}
	code, body := create(fresh, true)
	var p plan
	if json.Unmarshal([]byte(body), &p) != nil || code != 201 || p.State != "preparing" {
		t.Fatalf("create: %d %s", code, body)
	}
	if code, body := create(fresh, true); code == 201 {
		t.Fatalf("a second open plan on the same computer: %d %s", code, body)
	}
	// The computer's agent gets the prepare run; the code is never in the API answer.
	if strings.Contains(body, "code") || strings.Contains(body, "token") {
		t.Fatalf("plan answer shows its code: %s", body)
	}
	prep, err := e.store.ClaimRun(e.ctx, fresh)
	if err != nil || prep == nil || prep.Kind != api.KindRecoveryPrepare || prep.RestorePlanID == nil || *prep.RestorePlanID != p.ID {
		t.Fatalf("prepare run: %+v %v", prep, err)
	}
	if err := e.store.FinishRun(e.ctx, fresh, prep.ID, api.RunResult{Status: api.StatusSuccess}); err != nil {
		t.Fatal(err)
	}
	if got := get(p.ID); got.State != "waiting" {
		t.Fatalf("after prepare: %+v", got)
	}
	var code1 string
	if err := e.pool.QueryRow(e.ctx, `SELECT code FROM restore_plans WHERE id=$1`, p.ID).Scan(&code1); err != nil || code1 == "" {
		t.Fatalf("plan code: %q %v", code1, err)
	}
	// The plan's code cannot enroll an ordinary agent (and is not used up by trying).
	if _, err := enroll("intrus", false, code1, nil, ""); err == nil {
		t.Fatal("plan code enrolled an ordinary agent")
	}
	// The recovery environment joins; its disk numbers differ from Windows'.
	recDisks := `[{"number":0,"model":"USB stick","size":32000000000,"sector_size":512,"style":"mbr","mbr_signature":7,"bus":"usb","partitions":[]},
	  {"number":1,"model":"Msft Virtual Disk","size":161061273600,"sector_size":512,"style":"gpt","gpt_disk_id":"aaaa-new","partitions":[]}]`
	rec, err := enroll("MINWINPC", true, code1, nil, recDisks)
	if err != nil {
		t.Fatal(err)
	}
	got := get(p.ID)
	if got.State != "restoring" {
		t.Fatalf("after the recovery environment joined: %+v", got)
	}
	var restore server.Run
	var rid int64
	if err := e.pool.QueryRow(e.ctx, `SELECT restore_run_id FROM restore_plans WHERE id=$1`, p.ID).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	restore, _ = e.store.GetRun(e.ctx, rid)
	if restore.Kind != api.KindImageRestore || restore.AgentID != rec || restore.TargetDisk == nil || *restore.TargetDisk != 1 ||
		!restore.RepairBoot || !restore.NewHardware || !strings.Contains(string(restore.ExpectDisk), "AAAA-NEW") {
		t.Fatalf("restore run: %+v %s", restore, restore.ExpectDisk)
	}
	if cl, _ := e.store.ClaimRun(e.ctx, rec); cl == nil || cl.ID != rid {
		t.Fatalf("recovery agent claims: %+v", cl)
	}
	if err := e.store.FinishRun(e.ctx, rec, rid, api.RunResult{Status: api.StatusSuccess, Message: "Wrote 2 partitions"}); err != nil {
		t.Fatal(err)
	}
	if got := get(p.ID); got.State != "rebooting" || !got.Writing {
		t.Fatalf("after restore: %+v", got)
	}
	// The restored Windows is the old computer: its agent checks in.
	if err := e.store.TouchAgent(e.ctx, old, api.PollRequest{Hostname: "PC-VECHI", OS: "windows", Version: "test"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := get(p.ID); got.State != "done" {
		t.Fatalf("after the restored computer checked in: %+v", got)
	}

	// Disk not found in the recovery environment: nothing written, back to Windows.
	failRecovery := func(name, disks string) (plan, int64) {
		code, body := create(fresh, true)
		var q plan
		if json.Unmarshal([]byte(body), &q) != nil || code != 201 {
			t.Fatalf("%s: create %d %s", name, code, body)
		}
		pr, _ := e.store.ClaimRun(e.ctx, fresh)
		_ = e.store.FinishRun(e.ctx, fresh, pr.ID, api.RunResult{Status: api.StatusSuccess})
		var cd string
		_ = e.pool.QueryRow(e.ctx, `SELECT code FROM restore_plans WHERE id=$1`, q.ID).Scan(&cd)
		ra, err := enroll("MINWINPC", true, cd, nil, disks)
		if err != nil {
			t.Fatal(err)
		}
		return get(q.ID), ra
	}
	q, ra := failRecovery("no disk", `[{"number":0,"model":"Other","size":161061273600,"sector_size":512,"style":"gpt","gpt_disk_id":"BBBB","partitions":[]}]`)
	if q.State != "failed" || q.Writing || !strings.Contains(q.Message, "nothing was written") {
		t.Fatalf("no disk: %+v", q)
	}
	if back, _ := e.store.ClaimRun(e.ctx, ra); back == nil || back.Kind != api.KindRecoveryReboot {
		t.Fatalf("no disk: no way back to Windows: %+v", back)
	}
	// Two identical disks: not certain, refused the same way.
	two := `[{"number":0,"model":"Msft Virtual Disk","size":161061273600,"sector_size":512,"style":"gpt","gpt_disk_id":"AAAA-NEW","partitions":[]},{"number":1,"model":"Msft Virtual Disk","size":161061273600,"sector_size":512,"style":"gpt","gpt_disk_id":"AAAA-NEW","partitions":[]}]`
	if q, _ := failRecovery("two disks", two); q.State != "failed" || !strings.Contains(q.Message, "2 matches") {
		t.Fatalf("two disks: %+v", q)
	}
	// A restore that stops before writing (missing block): back to Windows.
	q, ra = failRecovery("ok", `[{"number":2,"model":"Msft Virtual Disk","size":161061273600,"sector_size":512,"style":"gpt","gpt_disk_id":"AAAA-NEW","partitions":[]}]`)
	if q.State != "restoring" {
		t.Fatalf("restoring: %+v", q)
	}
	rr, _ := e.store.ClaimRun(e.ctx, ra)
	_ = e.store.FinishRun(e.ctx, ra, rr.ID, api.RunResult{Status: api.StatusFailed, Message: "the copy cannot be restored, nothing was written: partition 3 block 9 is missing from the copy"})
	if got := get(q.ID); got.State != "failed" || got.Writing {
		t.Fatalf("missing block: %+v", got)
	}
	if back, _ := e.store.ClaimRun(e.ctx, ra); back == nil || back.Kind != api.KindRecoveryReboot {
		t.Fatalf("missing block: no way back: %+v", back)
	}

	// Cancel while waiting: the code stops working.
	code, body = create(fresh, true)
	_ = json.Unmarshal([]byte(body), &q)
	pr, _ := e.store.ClaimRun(e.ctx, fresh)
	_ = e.store.FinishRun(e.ctx, fresh, pr.ID, api.RunResult{Status: api.StatusSuccess})
	var cd string
	_ = e.pool.QueryRow(e.ctx, `SELECT code FROM restore_plans WHERE id=$1`, q.ID).Scan(&cd)
	if code, body := apiJSONCall(t, e, op, "POST", fmt.Sprintf("/api/v1/restore-plans/%d/cancel", q.ID), ""); (code != 200 && code != 202) || !strings.Contains(body, `"cancelled"`) {
		t.Fatalf("cancel: %d %s", code, body)
	}
	if _, err := enroll("MINWINPC", true, cd, nil, ""); err == nil {
		t.Fatal("the code of a cancelled plan still works")
	}

	// The recovery environment never joined: the computer's Windows checks in
	// again long after preparing, the plan fails, nothing was written.
	code, body = create(fresh, true)
	_ = json.Unmarshal([]byte(body), &q)
	pr, _ = e.store.ClaimRun(e.ctx, fresh)
	_ = e.store.FinishRun(e.ctx, fresh, pr.ID, api.RunResult{Status: api.StatusSuccess})
	if _, err := e.pool.Exec(e.ctx, `UPDATE restore_plans SET updated_at=now()-interval '20 minutes' WHERE id=$1`, q.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.TouchAgent(e.ctx, fresh, api.PollRequest{Hostname: "PC-NOU", OS: "windows", Version: "test", Capabilities: caps}, ""); err != nil {
		t.Fatal(err)
	}
	if got := get(q.ID); got.State != "failed" || got.Writing || !strings.Contains(got.Message, "network cable") {
		t.Fatalf("never joined: %+v", got)
	}
}
