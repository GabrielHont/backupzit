// Command backupzit-tray shows the BackupZit agent in the notification area
// (next to the clock): its state as icon colour, a menu with "Back up now"
// and a status window. It talks to the agent service over a local pipe and
// runs once per logged-on user.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/max-zit/backupzit/internal/localipc"
	"golang.org/x/sys/windows"
)

var (
	//go:embed icons/tray-ok.ico
	icoOK []byte
	//go:embed icons/tray-running.ico
	icoRunning []byte
	//go:embed icons/tray-warning.ico
	icoWarning []byte
	//go:embed icons/tray-error.ico
	icoError []byte
	//go:embed icons/tray-offline.ico
	icoOffline []byte
)

var stateIcon = map[string][]byte{"ok": icoOK, "running": icoRunning, "warning": icoWarning, "error": icoError, "offline": icoOffline, "idle": icoOK}

var version = "dev"

const maxJobItems = 12

type tray struct {
	mu   sync.Mutex
	sum  Summary
	icon string
	lang string

	status   *systray.MenuItem
	detail   *systray.MenuItem
	open     *systray.MenuItem
	backup   *systray.MenuItem
	jobItems []*systray.MenuItem
	jobIDs   []int64
	language *systray.MenuItem
	langRO   *systray.MenuItem
	langEN   *systray.MenuItem
	hide     *systray.MenuItem
	win      *statusWindow
}

func main() {
	show := len(os.Args) > 1 && (os.Args[1] == "--show" || os.Args[1] == "/show")
	if isServiceSession() {
		return // never in session 0 (e.g. started by an installer running as SYSTEM)
	}
	// One instance per session; a second start opens the window of the first.
	showEvent, _ := windows.CreateEvent(nil, 0, 0, windows.StringToUTF16Ptr(`Local\BackupZitTrayShow`))
	m, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(`Local\BackupZitTray`))
	if err == windows.ERROR_ALREADY_EXISTS {
		_ = windows.SetEvent(showEvent)
		return
	}
	defer windows.CloseHandle(m)

	t := &tray{win: newStatusWindow(), lang: loadLang()}
	systray.Run(func() { t.ready(showEvent, show) }, func() {})
}

// isServiceSession reports whether this process runs in session 0.
func isServiceSession() bool {
	var sid uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &sid); err != nil {
		return false
	}
	return sid == 0
}

func (t *tray) ready(showEvent windows.Handle, show bool) {
	systray.SetIcon(icoOffline)
	systray.SetTitle("Unirea Backup")
	systray.SetTooltip("Unirea Backup")
	t.status = systray.AddMenuItem("Unirea Backup", "")
	t.status.Disable()
	t.detail = systray.AddMenuItem("", "")
	t.detail.Disable()
	systray.AddSeparator()
	t.open = systray.AddMenuItem("", "")
	t.backup = systray.AddMenuItem("", "")
	for i := 0; i < maxJobItems; i++ {
		it := t.backup.AddSubMenuItem("", "")
		it.Hide()
		t.jobItems = append(t.jobItems, it)
		t.jobIDs = append(t.jobIDs, 0)
		go func(i int, it *systray.MenuItem) {
			for range it.ClickedCh {
				t.mu.Lock()
				id := t.jobIDs[i]
				t.mu.Unlock()
				if id != 0 {
					go t.runJob(id)
				}
			}
		}(i, it)
	}
	systray.AddSeparator()
	// Named in both languages so anyone finds it.
	t.language = systray.AddMenuItem("Limbă / Language", "")
	t.langRO = t.language.AddSubMenuItemCheckbox("Română", "", t.lang == langRO)
	t.langEN = t.language.AddSubMenuItemCheckbox("English", "", t.lang == langEN)
	t.hide = systray.AddMenuItem("", "")
	t.menuTexts()

	systray.SetOnTapped(func() { t.win.show(t) })
	go func() {
		for {
			select {
			case <-t.open.ClickedCh:
				t.win.show(t)
			case <-t.langRO.ClickedCh:
				t.setLang(langRO)
			case <-t.langEN.ClickedCh:
				t.setLang(langEN)
			case <-t.hide.ClickedCh:
				t.win.close()
				systray.Quit()
				return
			}
		}
	}()
	if showEvent != 0 {
		go func() {
			for {
				if ev, _ := windows.WaitForSingleObject(showEvent, windows.INFINITE); ev == windows.WAIT_OBJECT_0 {
					t.win.show(t)
				}
			}
		}()
	}
	go t.loop()
	if show {
		t.win.show(t)
	}
}

// menuTexts sets the fixed menu texts in the chosen language.
func (t *tray) menuTexts() {
	t.mu.Lock()
	l := t.lang
	t.mu.Unlock()
	t.open.SetTitle(tr(l, "Deschide Unirea Backup Agent", "Open Unirea Backup Agent"))
	t.open.SetTooltip(tr(l, "Arată starea backupului acestui calculator", "Show the status of this computer's backups"))
	t.backup.SetTitle(tr(l, "Fă backup acum", "Back up now"))
	t.backup.SetTooltip(tr(l, "Pornește acum un backup", "Start a backup job now"))
	t.hide.SetTitle(tr(l, "Ascunde iconița", "Hide this icon"))
	t.hide.SetTooltip(tr(l, "Iconița revine la următoarea conectare; backupul continuă în fundal",
		"The icon returns at the next sign-in; backups continue in the background"))
	if l == langEN {
		t.langRO.Uncheck()
		t.langEN.Check()
	} else {
		t.langEN.Uncheck()
		t.langRO.Check()
	}
}

// setLang switches the language of the menu and the window and keeps the
// choice for this Windows user.
func (t *tray) setLang(l string) {
	l = normLang(l)
	t.mu.Lock()
	t.lang = l
	t.mu.Unlock()
	saveLang(l)
	t.menuTexts()
	t.win.update(t.refresh())
}

// refresh asks the agent for its status and updates icon and menu.
func (t *tray) refresh() Summary {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := localipc.Call(ctx, localipc.Request{Cmd: localipc.CmdStatus})
	var st *localipc.Status
	if err == nil {
		st = resp.Status
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	sum := summarize(st, err, time.Now(), t.lang)
	l := sum.Lang
	t.sum = sum
	if sum.State != t.icon {
		systray.SetIcon(stateIcon[sum.State])
		t.icon = sum.State
	}
	systray.SetTooltip(sum.tooltip())
	head := sum.Headline
	if sum.State == "running" && sum.Percent >= 0 {
		head = fmt.Sprintf("%s — %d%%", head, sum.Percent)
	}
	t.status.SetTitle(head)
	if sum.Status != nil {
		t.detail.SetTitle(tr(l, "Ultimul: ", "Last: ") + sum.LastText + tr(l, "   Următorul: ", "   Next: ") + sum.NextText)
		t.detail.Show()
	} else {
		t.detail.Hide()
	}
	n := 0
	for _, j := range sum.Jobs {
		if n == maxJobItems {
			break
		}
		title := j.Name
		if j.Running {
			title += tr(l, "  (în curs)", "  (running)")
		}
		t.jobItems[n].SetTitle(title)
		t.jobItems[n].SetTooltip(j.KindText + tr(l, " · ultimul ", " · last ") + j.LastText + tr(l, " · următorul ", " · next ") + j.NextText)
		if j.Running {
			t.jobItems[n].Disable()
		} else {
			t.jobItems[n].Enable()
		}
		t.jobItems[n].Show()
		t.jobIDs[n] = j.ID
		n++
	}
	for i := n; i < maxJobItems; i++ {
		t.jobItems[i].Hide()
		t.jobIDs[i] = 0
	}
	if n == 0 {
		t.backup.Disable()
	} else {
		t.backup.Enable()
	}
	return sum
}

func (t *tray) loop() {
	for {
		sum := t.refresh()
		t.win.update(sum)
		every := 10 * time.Second
		if sum.State == "running" || t.win.isOpen() {
			every = 2 * time.Second
		}
		time.Sleep(every)
	}
}

func (t *tray) summary() Summary {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sum
}

// runJob starts a job now; errors are shown in the window and tooltip.
func (t *tray) runJob(id int64) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := localipc.Call(ctx, localipc.Request{Cmd: localipc.CmdRun, JobID: id}); err != nil {
		l := t.summary().Lang
		msg := tr(l, "Backupul nu a pornit: ", "The backup did not start: ") + strings.TrimSpace(err.Error())
		systray.SetTooltip(Summary{Lang: l, Headline: msg}.tooltip())
		return msg
	}
	go func() {
		time.Sleep(time.Second)
		t.win.update(t.refresh())
	}()
	return ""
}

// showMessage shows the status in a plain message box, for computers
// without the WebView2 runtime the window needs.
func (t *tray) showMessage() {
	s := t.summary()
	text, _ := windows.UTF16PtrFromString(s.Headline + "\n\n" + s.Detail)
	title, _ := windows.UTF16PtrFromString("Unirea Backup Agent")
	const mbIconInformation = 0x40
	_, _ = windows.MessageBox(0, text, title, mbIconInformation)
}
