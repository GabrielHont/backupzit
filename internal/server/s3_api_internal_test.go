package server

import (
	"net/url"
	"testing"

	"github.com/max-zit/backupzit/internal/repo"
)

func TestS3PrefixCannotOverrideTLS(t *testing.T) {
	got, err := s3Location("storage.test:9000", "backups", "office?tls=false#part", false)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.Path != "/backups/office?tls=false#part" {
		t.Fatalf("prefix changed transport settings: %s", got)
	}
	for _, p := range []string{"../other-bucket", "office/../other", "x\x00y"} {
		if _, err := s3Location("storage.test", "backups", p, false); err == nil {
			t.Errorf("accepted prefix %q", p)
		}
	}
}

func TestYearlyOnlyRetentionText(t *testing.T) {
	p := repo.RetentionPolicy{KeepYearly: 5}
	if got := retentionText(languages[0], p); got != p.String() {
		t.Fatalf("yearly retention: %s", got)
	}
}
