package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/max-zit/backupzit/internal/localipc"
)

// Summary is the interpreted agent status shown in the tray and window.
type Summary struct {
	State    string `json:"state"` // ok, running, warning, error, offline, idle
	Headline string `json:"headline"`
	Detail   string `json:"detail"`
	Percent  int    `json:"percent"` // -1 if unknown
	LastText string `json:"last_text"`
	NextText string `json:"next_text"`
	// Status is the raw status from the agent (nil if the service is not
	// reachable).
	Status *localipc.Status `json:"status"`
	Jobs   []JobView        `json:"jobs"`
	Error  string           `json:"error,omitempty"`
	// ActivityText is the last run of any kind, or the running one.
	ActivityText string `json:"activity_text"`
}

// JobView is a job with display texts.
type JobView struct {
	localipc.Job
	KindText     string `json:"kind_text"`
	LastText     string `json:"last_text"`
	NextText     string `json:"next_text"`
	StatusText   string `json:"status_text"`
	ScheduleText string `json:"schedule_text"`
}

// The texts are in Romanian: the agent is installed by Unirea Cloud's
// customers, who are Romanian firms.

var kindText = map[string]string{"files": "Fișiere", "image": "Întregul calculator", "vm": "Mașini virtuale Proxmox", "copy": "Copie a backupului"}

var runKindText = map[string]string{
	"backup": "Backup de fișiere", "image-backup": "Backup al întregului calculator", "vm-backup": "Backup de mașini virtuale", "copy": "Copie a backupului",
	"restore": "Refacere de fișiere", "image-restore": "Refacerea calculatorului", "image-file-restore": "Refacere de fișiere", "vm-restore": "Refacere de mașină virtuală",
	"verify": "Test de refacere", "agent-update": "Actualizarea programului",
	"recovery-prepare": "Pregătirea refacerii", "recovery-reboot": "Repornire pentru refacere",
}

var statusText = map[string]string{
	"queued": "în așteptare", "running": "în curs", "success": "reușit", "warning": "cu avertismente",
	"failed": "eșuat", "cancelled": "oprit",
}

func runName(kind string) string {
	if n := runKindText[kind]; n != "" {
		return n
	}
	return kind
}

func statusName(st string) string {
	if n := statusText[st]; n != "" {
		return n
	}
	return st
}

var weekdays = [...]string{"dum", "lun", "mar", "mie", "joi", "vin", "sâm"}
var months = [...]string{"ian", "feb", "mar", "apr", "mai", "iun", "iul", "aug", "sep", "oct", "nov", "dec"}

// when renders a time relative to now: "azi 14:05", "ieri 22:00",
// "lun 5 oct 22:00".
func when(t time.Time, now time.Time) string {
	t = t.Local()
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.Local) }
	diff := int(day(y1, m1, d1).Sub(day(y2, m2, d2)).Hours() / 24)
	switch diff {
	case 0:
		return "azi " + t.Format("15:04")
	case -1:
		return "ieri " + t.Format("15:04")
	case 1:
		return "mâine " + t.Format("15:04")
	}
	return fmt.Sprintf("%s %d %s %s", weekdays[t.Weekday()], t.Day(), months[t.Month()-1], t.Format("15:04"))
}

// scheduleText turns the simple cron schedules the portal sets into words
// ("zilnic la 21:00"); anything else is shown as it is.
func scheduleText(cron string) string {
	f := strings.Fields(cron)
	if len(f) != 5 || f[2] != "*" || f[3] != "*" {
		if cron == "" || strings.EqualFold(cron, "manual") {
			return "doar manual"
		}
		return cron
	}
	m, err1 := strconv.Atoi(f[0])
	h, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || m < 0 || m > 59 || h < 0 || h > 23 {
		return cron
	}
	at := fmt.Sprintf("la %02d:%02d", h, m)
	switch f[4] {
	case "*":
		return "zilnic " + at
	case "1-5":
		return "luni–vineri " + at
	}
	if d, err := strconv.Atoi(f[4]); err == nil && d >= 0 && d <= 7 {
		return "în fiecare " + [...]string{"duminică", "luni", "marți", "miercuri", "joi", "vineri", "sâmbătă", "duminică"}[d] + " " + at
	}
	return cron
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	// Romanian decimal comma: 1,5 GiB.
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return strings.Replace(fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp]), ".", ",", 1)
}

func summarize(st *localipc.Status, callErr error, now time.Time) Summary {
	s := Summary{Percent: -1, Jobs: []JobView{}}
	if st == nil {
		s.State, s.Headline = "offline", "Programul de backup nu rulează"
		s.Detail = "Porniți serviciul „Unirea Backup Agent” din Servicii sau reinstalați programul."
		if callErr != nil {
			s.Error = callErr.Error()
		}
		return s
	}
	s.Status = st
	switch {
	case st.Current != nil:
		s.ActivityText = runName(st.Current.Kind) + " — în curs"
	case st.Last != nil:
		name := st.Last.Job
		if name == "" {
			name = runName(st.Last.Kind)
		}
		s.ActivityText = name + " — " + statusName(st.Last.Status)
	}
	if !st.Enrolled {
		s.State, s.Headline = "offline", "Calculatorul nu e încă înscris"
		switch {
		case st.ServerURL != "" && st.LastError != "":
			s.Detail = "Înscrierea la " + st.ServerURL + " nu a reușit: " + st.LastError + ". Luați un cod nou din portalul Unirea Cloud și rulați din nou programul de instalare."
		default:
			s.Detail = "Rulați din nou programul de instalare și lipiți codul de înscriere din portalul Unirea Cloud."
		}
		return s
	}

	// Newest finished backup and next scheduled run over all jobs.
	var lastJob *localipc.Job
	var next *time.Time
	failed, warned := 0, 0
	for i := range st.Jobs {
		j := st.Jobs[i]
		v := JobView{Job: j, KindText: kindText[j.Kind], StatusText: statusName(j.LastStatus), ScheduleText: scheduleText(j.Schedule)}
		if v.KindText == "" {
			v.KindText = j.Kind
		}
		if j.LastFinished != nil {
			v.LastText = when(*j.LastFinished, now)
			if lastJob == nil || j.LastFinished.After(*lastJob.LastFinished) {
				lastJob = &st.Jobs[i]
			}
		} else {
			v.LastText = "niciodată"
		}
		switch j.LastStatus {
		case "failed":
			failed++
		case "warning":
			warned++
		}
		if j.NextRun != nil {
			v.NextText = when(*j.NextRun, now)
			if next == nil || j.NextRun.Before(*next) {
				next = j.NextRun
			}
		} else if !j.Enabled {
			v.NextText = "oprit"
		} else {
			v.NextText = "doar manual"
		}
		s.Jobs = append(s.Jobs, v)
	}
	if lastJob != nil {
		s.LastText = fmt.Sprintf("%s — %s, %s", when(*lastJob.LastFinished, now), lastJob.Name, statusName(lastJob.LastStatus))
	} else {
		s.LastText = "niciun backup încă"
	}
	if next != nil {
		s.NextText = when(*next, now)
	} else {
		s.NextText = "neprogramat"
	}

	switch {
	case st.Current != nil:
		c := st.Current
		s.State = "running"
		name := runName(c.Kind)
		if c.Job != "" {
			name += " — " + c.Job
		}
		s.Headline = name
		switch {
		case c.Total > 0:
			s.Percent = int(c.Done * 100 / c.Total) // #nosec G115 -- progress percentage, done <= total
			s.Detail = fmt.Sprintf("%d%% · %s citiți din %s · început %s", s.Percent, humanBytes(c.Done), humanBytes(c.Total), when(c.Started, now))
		case c.Files > 0 && c.Done > 0:
			s.Detail = fmt.Sprintf("%d fișiere · %s citiți · început %s", c.Files, humanBytes(c.Done), when(c.Started, now))
		case c.Files > 0:
			s.Detail = fmt.Sprintf("%d fișiere verificate · început %s", c.Files, when(c.Started, now))
		default:
			s.Detail = "început " + when(c.Started, now)
		}
	case !st.Connected && st.LastContact == nil:
		s.State, s.Headline = "offline", "Fără legătură cu serverul de backup"
		s.Detail = strings.TrimSpace("Încerc să ajung la " + st.ServerURL + ". " + st.LastError)
	case len(st.Jobs) == 0 && st.JobsAt != nil:
		s.State, s.Headline = "idle", "Calculatorul nu are încă backup programat"
		s.Detail = "Programați backupul pentru " + st.Hostname + " din portalul Unirea Cloud."
	case failed > 0:
		s.State, s.Headline = "error", "Ultimul backup nu a reușit"
		if failed > 1 {
			s.Headline = fmt.Sprintf("%d backupuri nu au reușit", failed)
		}
		s.Detail = "Detaliile sunt în portalul Unirea Cloud."
		for _, j := range st.Jobs {
			if j.LastStatus == "failed" && j.LastMessage != "" {
				s.Detail = j.Name + ": " + j.LastMessage
				break
			}
		}
	case warned > 0:
		s.State, s.Headline = "warning", "Backup făcut, cu avertismente"
		s.Detail = "Câteva fișiere nu au putut fi citite. Lista e în portalul Unirea Cloud."
	case lastJob != nil:
		s.State, s.Headline = "ok", "Calculatorul e protejat"
		s.Detail = "Ultimul backup: " + when(*lastJob.LastFinished, now) + "."
	default:
		s.State, s.Headline = "idle", "Se așteaptă primul backup"
		s.Detail = "Următorul backup: " + s.NextText + "."
	}
	if !st.Connected && st.LastContact != nil && st.Current == nil {
		s.State = "offline"
		s.Detail = "Fără legătură cu serverul de backup din " + when(*st.LastContact, now) + ". Backupurile programate pornesc când revine legătura. " + s.Detail
	}
	return s
}

// tooltip is the short text shown on hover (Windows limits it to 127
// characters).
//
//lint:ignore U1000 used by the Windows tray
func (s Summary) tooltip() string {
	t := "Unirea Backup — " + s.Headline
	if s.State == "running" && s.Percent >= 0 {
		t += fmt.Sprintf(" (%d%%)", s.Percent)
	} else if s.Status != nil && s.LastText != "" {
		t += "\nUltimul: " + s.LastText
	}
	// Cut on a character boundary (the texts have Romanian letters).
	if utf8.RuneCountInString(t) > 127 {
		r := []rune(t)
		t = string(r[:124]) + "..."
	}
	return t
}
