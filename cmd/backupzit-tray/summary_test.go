package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/max-zit/backupzit/internal/localipc"
)

// A running service that is not enrolled is not reported as "not running".
func TestSummaryNotEnrolled(t *testing.T) {
	st := &localipc.Status{ServerURL: "https://backup:443", LastError: "certificate mismatch"}
	if s := summarize(st, nil, time.Now(), langRO); s.Headline != "Calculatorul nu e încă înscris" || !strings.Contains(s.Detail, "cod nou") {
		t.Fatalf("%+v", s)
	}
	if s := summarize(st, nil, time.Now(), langEN); s.Headline != "This computer is not enrolled yet" {
		t.Fatalf("en: %q", s.Headline)
	}
	if s := summarize(nil, nil, time.Now(), ""); s.Headline != "Programul de backup nu rulează" || s.Lang != langRO {
		t.Fatalf("no service, default language: %q %q", s.Headline, s.Lang)
	}
	if s := summarize(nil, nil, time.Now(), langEN); s.Headline != "Agent service is not running" {
		t.Fatalf("no service: %q", s.Headline)
	}
}

// The console sends English schedule descriptions (Schedule.Describe).
func TestScheduleText(t *testing.T) {
	for in, want := range map[string]string{
		"Every day at 21:00":                          "Zilnic la 21:00",
		"Every day at 08:00, 20:00":                   "Zilnic la 08:00, 20:00",
		"Mon–Fri at 12:30":                            "Luni–vineri la 12:30",
		"Sat and Sun at 10:00":                        "Sâmbăta și duminica la 10:00",
		"On Mon, Wed, Fri at 22:00":                   "În zilele lun, mie, vin la 22:00",
		"Every 15 minutes, every day":                 "La fiecare 15 minute, zilnic",
		"Every hour between 08:00 and 18:00, Mon–Fri": "La fiecare oră între 08:00 și 18:00, luni–vineri",
		"Every 4 hours, on Sat":                       "La fiecare 4 ore, în zilele sâm",
		"Monthly on day 1 at 03:00":                   "Lunar, pe 1, la 03:00",
		"Manual only":                                 "Doar manual",
		"cron: */5 * * * *":                           "cron: */5 * * * *",
	} {
		if got := scheduleText(in, langRO); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
		if got := scheduleText(in, langEN); got != in {
			t.Errorf("en %q: got %q", in, got)
		}
	}
}

func TestWhen(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.Local)
	old := time.Date(2026, 11, 9, 22, 5, 0, 0, time.Local)
	if got := when(old, now, langRO); got != "lun 9 nov 22:05" {
		t.Errorf("ro: %q", got)
	}
	if got := when(old, now, langEN); got != "Mon 9 Nov 22:05" {
		t.Errorf("en: %q", got)
	}
	if got := when(now.Add(-24*time.Hour), now, langRO); got != "ieri 12:00" {
		t.Errorf("ro yesterday: %q", got)
	}
	if got := humanBytes(3<<29, langRO); got != "1,5 GiB" {
		t.Errorf("ro bytes: %q", got)
	}
	if got := humanBytes(3<<29, langEN); got != "1.5 GiB" {
		t.Errorf("en bytes: %q", got)
	}
}

func TestTooltipCutsOnCharacters(t *testing.T) {
	s := Summary{Headline: strings.Repeat("ă", 200)}
	if tt := s.tooltip(); !utf8.ValidString(tt) || utf8.RuneCountInString(tt) > 127 {
		t.Fatalf("bad tooltip %q", tt)
	}
}

func TestLangFile(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	if l := loadLang(); l != langRO {
		t.Fatalf("default %q", l)
	}
	saveLang(langEN)
	if l := loadLang(); l != langEN {
		t.Fatalf("saved %q", l)
	}
	saveLang("xx")
	if l := loadLang(); l != langRO {
		t.Fatalf("unknown %q", l)
	}
}

// Customers do not see the backup server's address.
func TestNoServerAddressShown(t *testing.T) {
	last := time.Now().Add(-time.Hour)
	for _, st := range []*localipc.Status{
		{ServerURL: "https://backup.example:18951", LastError: "dial tcp backup.example:18951: refused"},
		{ServerURL: "https://backup.example:18951", Enrolled: true, LastError: "dial tcp backup.example:18951: refused"},
		{ServerURL: "https://backup.example:18951", Enrolled: true, LastContact: &last, LastError: "dial tcp backup.example:18951: refused"},
	} {
		for _, l := range []string{langRO, langEN} {
			s := summarize(st, nil, time.Now(), l)
			if strings.Contains(s.Headline+s.Detail, "backup.example") {
				t.Errorf("%s: address shown: %q / %q", l, s.Headline, s.Detail)
			}
			w := s.forWindow()
			if w.Status.ServerURL != "" || w.Status.LastError != "" || s.Status.ServerURL == "" {
				t.Errorf("forWindow: %+v (original %+v)", w.Status, s.Status)
			}
		}
	}
}
