package repo

import (
	"testing"
	"time"
)

func TestYearlyRetention(t *testing.T) {
	loc := time.FixedZone("UTC+2", 2*60*60)
	var sns []*Snapshot
	for _, date := range []string{"2023-06-01T00:00:00Z", "2024-06-01T00:00:00Z", "2024-12-31T23:30:00Z", "2025-06-01T00:00:00Z", "2026-01-01T00:00:00Z"} {
		d, _ := time.Parse(time.RFC3339, date)
		sns = append(sns, &Snapshot{Time: d})
	}
	p := RetentionPolicy{KeepYearly: 3}
	keep, remove := ApplyPolicy(sns, p, loc)
	if len(keep) != 3 || len(remove) != 2 {
		t.Fatalf("kept %d, removed %d", len(keep), len(remove))
	}
	if keep[0] != sns[4] || keep[1] != sns[3] || keep[2] != sns[1] {
		t.Fatalf("did not select the newest snapshot of each local year")
	}
	if p.Empty() || p.String() != "keep 3 yearly" {
		t.Fatal(p.String())
	}
}
