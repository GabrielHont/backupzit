package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/max-zit/backupzit/internal/localipc"
)

// Summary is the interpreted agent status shown in the tray and window.
type Summary struct {
	Lang     string `json:"lang"`  // "ro" or "en"
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

// The tray speaks Romanian (Unirea Cloud's customers are Romanian firms)
// or English, as chosen in the window or the menu.
const (
	langRO = "ro"
	langEN = "en"
)

func normLang(l string) string {
	if l == langEN {
		return langEN
	}
	return langRO
}

// tr picks the text of the language.
func tr(lang, ro, en string) string {
	if lang == langEN {
		return en
	}
	return ro
}

type pair struct{ ro, en string }

var kindText = map[string]pair{
	"files": {"Fișiere", "Files"}, "image": {"Întregul calculator", "Disk image"},
	"vm": {"Mașini virtuale Proxmox", "Proxmox VMs"}, "copy": {"Copie a backupului", "Backup copy"},
}

var runKindText = map[string]pair{
	"backup": {"Backup de fișiere", "Backup"}, "image-backup": {"Backup al întregului calculator", "Disk image backup"},
	"vm-backup": {"Backup de mașini virtuale", "VM backup"}, "copy": {"Copie a backupului", "Backup copy"},
	"restore": {"Refacere de fișiere", "Restore"}, "image-restore": {"Refacerea calculatorului", "Disk restore"},
	"image-file-restore": {"Refacere de fișiere", "File restore"}, "vm-restore": {"Refacere de mașină virtuală", "VM restore"},
	"verify": {"Test de refacere", "Restore test"}, "agent-update": {"Actualizarea programului", "Agent update"},
	"recovery-prepare": {"Pregătirea refacerii", "Restore preparation"}, "recovery-reboot": {"Repornire pentru refacere", "Restart for restore"},
}

var statusText = map[string]pair{
	"queued": {"în așteptare", "queued"}, "running": {"în curs", "running"}, "success": {"reușit", "success"},
	"warning": {"cu avertismente", "warning"}, "failed": {"eșuat", "failed"}, "cancelled": {"oprit", "cancelled"},
}

func pick(m map[string]pair, key, lang string) string {
	if p, ok := m[key]; ok {
		return tr(lang, p.ro, p.en)
	}
	return key
}

var (
	weekdaysRO = [...]string{"dum", "lun", "mar", "mie", "joi", "vin", "sâm"}
	monthsRO   = [...]string{"ian", "feb", "mar", "apr", "mai", "iun", "iul", "aug", "sep", "oct", "nov", "dec"}
)

// when renders a time relative to now: "azi 14:05" / "today 14:05",
// "lun 5 oct 22:00" / "Mon 5 Oct 22:00".
func when(t time.Time, now time.Time, lang string) string {
	t = t.Local()
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.Local) }
	diff := int(day(y1, m1, d1).Sub(day(y2, m2, d2)).Hours() / 24)
	hm := t.Format("15:04")
	switch diff {
	case 0:
		return tr(lang, "azi ", "today ") + hm
	case -1:
		return tr(lang, "ieri ", "yesterday ") + hm
	case 1:
		return tr(lang, "mâine ", "tomorrow ") + hm
	}
	if lang == langEN {
		return t.Format("Mon 2 Jan 15:04")
	}
	return fmt.Sprintf("%s %d %s %s", weekdaysRO[t.Weekday()], t.Day(), monthsRO[t.Month()-1], hm)
}

func humanBytes(b uint64, lang string) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	s := fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
	if lang == langRO {
		s = strings.Replace(s, ".", ",", 1) // Romanian decimal comma: 1,5 GiB
	}
	return s
}

var (
	dayRO = map[string]string{"Sun": "dum", "Mon": "lun", "Tue": "mar", "Wed": "mie", "Thu": "joi", "Fri": "vin", "Sat": "sâm"}
	// The console sends its English schedule description (Schedule.Describe).
	reDaily    = regexp.MustCompile(`^(Every day|every day|Mon–Fri|Sat and Sun|[Oo]n [A-Z][a-z]{2}(?:, [A-Z][a-z]{2})*) at (.+)$`)
	reInterval = regexp.MustCompile(`^Every (?:(\d+) (minutes|hours)|hour)(?: between (\d\d:00) and (\d\d:00))?, (every day|Mon–Fri|Sat and Sun|on [A-Z][a-z]{2}(?:, [A-Z][a-z]{2})*)$`)
	reMonthly  = regexp.MustCompile(`^Monthly on day (\d+) at (.+)$`)
)

func daysRO(d string) string {
	switch d {
	case "Every day", "every day":
		return "zilnic"
	case "Mon–Fri":
		return "luni–vineri"
	case "Sat and Sun":
		return "sâmbăta și duminica"
	}
	names := strings.Split(strings.TrimPrefix(strings.TrimPrefix(d, "on "), "On "), ", ")
	for i, n := range names {
		if r, ok := dayRO[n]; ok {
			names[i] = r
		}
	}
	return "în zilele " + strings.Join(names, ", ")
}

// scheduleText shows the console's schedule description in the language.
// Unknown forms (and custom cron schedules) are shown as they are.
func scheduleText(desc, lang string) string {
	if lang == langEN {
		return desc
	}
	if m := reDaily.FindStringSubmatch(desc); m != nil {
		r := []rune(daysRO(m[1]) + " la " + m[2])
		return strings.ToUpper(string(r[:1])) + string(r[1:])
	}
	if m := reInterval.FindStringSubmatch(desc); m != nil {
		var every string
		switch {
		case m[1] == "":
			every = "La fiecare oră"
		case m[2] == "minutes":
			every = "La fiecare " + m[1] + " minute"
		default:
			every = "La fiecare " + m[1] + " ore"
		}
		if m[3] != "" {
			every += " între " + m[3] + " și " + m[4]
		}
		return every + ", " + daysRO(m[5])
	}
	if m := reMonthly.FindStringSubmatch(desc); m != nil {
		return "Lunar, pe " + m[1] + ", la " + m[2]
	}
	switch desc {
	case "", "Manual only":
		return "Doar manual"
	case "After each backup of the source job":
		return "După fiecare backup al sursei"
	case "After another job finished":
		return "După terminarea altei sarcini"
	case "After another job succeeded":
		return "După reușita altei sarcini"
	}
	return desc
}

func summarize(st *localipc.Status, callErr error, now time.Time, lang string) Summary {
	lang = normLang(lang)
	L := func(ro, en string) string { return tr(lang, ro, en) }
	w := func(t time.Time) string { return when(t, now, lang) }
	s := Summary{Lang: lang, Percent: -1, Jobs: []JobView{}}
	if st == nil {
		s.State, s.Headline = "offline", L("Programul de backup nu rulează", "Agent service is not running")
		s.Detail = L("Porniți serviciul „Unirea Backup Agent” din Servicii sau reinstalați programul.",
			"Start the \"Unirea Backup Agent\" service or reinstall the agent.")
		if callErr != nil {
			s.Error = callErr.Error()
		}
		return s
	}
	s.Status = st
	switch {
	case st.Current != nil:
		s.ActivityText = pick(runKindText, st.Current.Kind, lang) + " — " + L("în curs", "running now")
	case st.Last != nil:
		name := st.Last.Job
		if name == "" {
			name = pick(runKindText, st.Last.Kind, lang)
		}
		s.ActivityText = name + " — " + pick(statusText, st.Last.Status, lang)
	}
	if !st.Enrolled {
		s.State, s.Headline = "offline", L("Calculatorul nu e încă înscris", "This computer is not enrolled yet")
		switch {
		case st.ServerURL != "" && st.LastError != "":
			s.Detail = L("Înscrierea la "+st.ServerURL+" nu a reușit: "+st.LastError+". Luați un cod nou din portalul Unirea Cloud și rulați din nou programul de instalare.",
				"Enrolling with "+st.ServerURL+" failed: "+st.LastError+". Get a new enrollment code in the Unirea Cloud portal and run the installer again.")
		default:
			s.Detail = L("Rulați din nou programul de instalare și lipiți codul de înscriere din portalul Unirea Cloud.",
				"Run the installer again and paste the enrollment code from the Unirea Cloud portal.")
		}
		return s
	}

	// Newest finished backup and next scheduled run over all jobs.
	var lastJob *localipc.Job
	var next *time.Time
	failed, warned := 0, 0
	for i := range st.Jobs {
		j := st.Jobs[i]
		v := JobView{Job: j, KindText: pick(kindText, j.Kind, lang), StatusText: pick(statusText, j.LastStatus, lang),
			ScheduleText: scheduleText(j.Schedule, lang)}
		if j.LastFinished != nil {
			v.LastText = w(*j.LastFinished)
			if lastJob == nil || j.LastFinished.After(*lastJob.LastFinished) {
				lastJob = &st.Jobs[i]
			}
		} else {
			v.LastText = L("niciodată", "never")
		}
		switch j.LastStatus {
		case "failed":
			failed++
		case "warning":
			warned++
		}
		if j.NextRun != nil {
			v.NextText = w(*j.NextRun)
			if next == nil || j.NextRun.Before(*next) {
				next = j.NextRun
			}
		} else if !j.Enabled {
			v.NextText = L("oprit", "disabled")
		} else {
			v.NextText = L("doar manual", "manual only")
		}
		s.Jobs = append(s.Jobs, v)
	}
	if lastJob != nil {
		s.LastText = fmt.Sprintf("%s — %s, %s", w(*lastJob.LastFinished), lastJob.Name, pick(statusText, lastJob.LastStatus, lang))
	} else {
		s.LastText = L("niciun backup încă", "no backup yet")
	}
	if next != nil {
		s.NextText = w(*next)
	} else {
		s.NextText = L("neprogramat", "not scheduled")
	}

	switch {
	case st.Current != nil:
		c := st.Current
		s.State = "running"
		name := pick(runKindText, c.Kind, lang)
		if c.Job != "" {
			name += " — " + c.Job
		}
		s.Headline = name
		hb := func(b uint64) string { return humanBytes(b, lang) }
		switch {
		case c.Total > 0:
			s.Percent = int(c.Done * 100 / c.Total) // #nosec G115 -- progress percentage, done <= total
			s.Detail = fmt.Sprintf(L("%d%% · %s citiți din %s · început %s", "%d%% · %s of %s read · started %s"), s.Percent, hb(c.Done), hb(c.Total), w(c.Started))
		case c.Files > 0 && c.Done > 0:
			s.Detail = fmt.Sprintf(L("%d fișiere · %s citiți · început %s", "%d files · %s read · started %s"), c.Files, hb(c.Done), w(c.Started))
		case c.Files > 0:
			s.Detail = fmt.Sprintf(L("%d fișiere verificate · început %s", "%d files checked · started %s"), c.Files, w(c.Started))
		default:
			s.Detail = L("început ", "started ") + w(c.Started)
		}
	case !st.Connected && st.LastContact == nil:
		s.State, s.Headline = "offline", L("Fără legătură cu serverul de backup", "Not connected to the backup server")
		s.Detail = strings.TrimSpace(L("Încerc să ajung la ", "Trying to reach ") + st.ServerURL + ". " + st.LastError)
	case len(st.Jobs) == 0 && st.JobsAt != nil:
		s.State, s.Headline = "idle", L("Calculatorul nu are încă backup programat", "No backup jobs for this computer")
		s.Detail = L("Programați backupul pentru "+st.Hostname+" din portalul Unirea Cloud.",
			"Create a backup job for "+st.Hostname+" in the Unirea Cloud portal.")
	case failed > 0:
		s.State, s.Headline = "error", L("Ultimul backup nu a reușit", "Last backup failed")
		if failed > 1 {
			s.Headline = fmt.Sprintf(L("%d backupuri nu au reușit", "%d backup jobs failed"), failed)
		}
		s.Detail = L("Detaliile sunt în portalul Unirea Cloud.", "The details are in the Unirea Cloud portal.")
		for _, j := range st.Jobs {
			if j.LastStatus == "failed" && j.LastMessage != "" {
				s.Detail = j.Name + ": " + j.LastMessage
				break
			}
		}
	case warned > 0:
		s.State, s.Headline = "warning", L("Backup făcut, cu avertismente", "Backed up with warnings")
		s.Detail = L("Câteva fișiere nu au putut fi citite. Lista e în portalul Unirea Cloud.",
			"Some files could not be read. The list is in the Unirea Cloud portal.")
	case lastJob != nil:
		s.State, s.Headline = "ok", L("Calculatorul e protejat", "This computer is protected")
		s.Detail = L("Ultimul backup: ", "Last backup: ") + w(*lastJob.LastFinished) + "."
	default:
		s.State, s.Headline = "idle", L("Se așteaptă primul backup", "Waiting for the first backup")
		s.Detail = L("Următorul backup: ", "Next backup: ") + s.NextText + "."
	}
	if !st.Connected && st.LastContact != nil && st.Current == nil {
		s.State = "offline"
		s.Detail = L("Fără legătură cu serverul de backup din "+w(*st.LastContact)+". Backupurile programate pornesc când revine legătura. ",
			"No connection to the backup server since "+w(*st.LastContact)+". Scheduled backups start once it is back. ") + s.Detail
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
		t += tr(s.Lang, "\nUltimul: ", "\nLast: ") + s.LastText
	}
	// Cut on a character boundary (the texts have Romanian letters).
	if utf8.RuneCountInString(t) > 127 {
		r := []rune(t)
		t = string(r[:124]) + "..."
	}
	return t
}
