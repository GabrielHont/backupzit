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
	s := summarize(&localipc.Status{ServerURL: "https://backup:443", LastError: "certificate mismatch"}, nil, time.Now())
	if s.Headline != "Calculatorul nu e încă înscris" || !strings.Contains(s.Detail, "certificate mismatch") {
		t.Fatalf("%+v", s)
	}
	if s := summarize(nil, nil, time.Now()); s.Headline != "Programul de backup nu rulează" {
		t.Fatalf("no service: %q", s.Headline)
	}
}

func TestScheduleText(t *testing.T) {
	for in, want := range map[string]string{
		"0 21 * * *": "zilnic la 21:00", "30 7 * * 1-5": "luni–vineri la 07:30", "0 22 * * 6": "în fiecare sâmbătă la 22:00",
		"Manual": "doar manual", "*/5 * * * *": "*/5 * * * *",
	} {
		if got := scheduleText(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestWhenRomanian(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.Local)
	if got := when(time.Date(2026, 11, 9, 22, 5, 0, 0, time.Local), now); got != "lun 9 nov 22:05" {
		t.Errorf("got %q", got)
	}
	if got := when(now.Add(-24*time.Hour), now); got != "ieri 12:00" {
		t.Errorf("got %q", got)
	}
}

func TestTooltipCutsOnCharacters(t *testing.T) {
	s := Summary{Headline: strings.Repeat("ă", 200)}
	if tt := s.tooltip(); !utf8.ValidString(tt) || utf8.RuneCountInString(tt) > 127 {
		t.Fatalf("bad tooltip %q", tt)
	}
}
