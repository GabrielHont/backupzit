//go:build windows

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/imaging"
	"golang.org/x/sys/windows"
)

func recoveryPrepareSupported() bool { return true }

// workDir holds the copy of Windows RE, its boot.sdi and the state needed to
// undo the preparation. It lives on the system drive, which the restore
// replaces: the copy runs from memory by then.
func workDir() string { return filepath.Join(systemDrive()+`\`, "BackupZitRecovery") }

func systemDrive() string {
	if d := os.Getenv("SystemDrive"); d != "" {
		return d
	}
	return "C:"
}

func inRecoveryEnvironment() bool { return strings.EqualFold(systemDrive(), "X:") }

type recoveryState struct {
	Loader  string `json:"loader"`  // BCD entry that starts the copy
	Options string `json:"options"` // its ramdisk options
}

var guidRe = regexp.MustCompile(`\{[0-9a-fA-F-]{36}\}`)

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(decodeConsole(out))
	if err != nil {
		return text, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, lastLines(text, 3))
	}
	return text, nil
}

// decodeConsole reads UTF-16 output (some tools write it) or bytes as they are.
func decodeConsole(b []byte) string {
	if len(b) >= 2 && b[1] == 0 {
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		}
		return string(utf16.Decode(u))
	}
	return string(b)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, " "))
}

func firmwareUEFI() bool {
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetFirmwareType")
	var t uint32
	if r, _, _ := proc.Call(uintptr(unsafe.Pointer(&t))); r == 0 {
		// Unknown: Windows 11 always starts through UEFI.
		return true
	}
	return t == 2 // FirmwareTypeUefi
}

func freeBytes(path string) uint64 {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	var free uint64
	if windows.GetDiskFreeSpaceEx(p, &free, nil, nil) != nil {
		return 0
	}
	return free
}

// findWinRE returns the computer's Windows RE image: in System32 when the
// recovery environment is disabled, otherwise on one of the system disk's
// partitions (\Recovery\WindowsRE\Winre.wim).
func findWinRE() (string, error) {
	cands := []string{filepath.Join(os.Getenv("SystemRoot"), `System32\Recovery\Winre.wim`)}
	if disks, err := imaging.ListDisks(); err == nil {
		for _, d := range disks {
			if !d.System {
				continue
			}
			for _, p := range d.Partitions {
				if p.VolumeGUIDPath != "" {
					cands = append(cands, strings.TrimRight(p.VolumeGUIDPath, `\`)+`\Recovery\WindowsRE\Winre.wim`)
				}
			}
		}
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && st.Size() > 100<<20 {
			return c, nil
		}
	}
	return "", errors.New("this Windows has no recovery environment (Winre.wim); the restore needs it")
}

func copyRecoveryFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// keepDrivers leaves only network and storage drivers among the exported
// ones: they are what the recovery environment and the restored Windows may
// lack; graphics and the rest only make the image larger.
func keepDrivers(dir string) (kept int) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		keep := false
		_ = filepath.WalkDir(sub, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(p), ".inf") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			sc := bufio.NewScanner(strings.NewReader(decodeConsole(b)))
			for sc.Scan() {
				l := strings.ToLower(strings.ReplaceAll(sc.Text(), " ", ""))
				if strings.HasPrefix(l, "class=net") || strings.HasPrefix(l, "class=scsiadapter") || strings.HasPrefix(l, "class=hdc") {
					keep = true
				}
			}
			return nil
		})
		if keep {
			kept++
		} else {
			_ = os.RemoveAll(sub)
		}
	}
	return kept
}

// bitlockerOn reports whether the system drive is protected by BitLocker
// (the PowerShell values are not translated).
func bitlockerOn(ctx context.Context) bool {
	out, err := run(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"try { (Get-BitLockerVolume -MountPoint $env:SystemDrive -ErrorAction Stop).ProtectionStatus } catch { 'Off' }")
	return err == nil && strings.Contains(out, "On")
}

const startScript = `@echo off
wpeinit
title Unirea Cloud - refacerea calculatorului
color 1F
echo Unirea Cloud reface calculatorul din copie. Nu opri calculatorul.
echo Pornesc reteaua...
wpeutil WaitForNetwork >nul 2>&1
X:\Windows\System32\backupzit-agent.exe recovery --plan X:\BackupZit\plan.json
echo.
echo Repornesc calculatorul...
wpeutil reboot
`

func prepareRecoveryStart(ctx context.Context, plan api.RecoveryPlan, log func(string)) ([]string, error) {
	if inRecoveryEnvironment() {
		return nil, errors.New("already in a recovery environment")
	}
	var notes []string
	dir := workDir()
	cleanupRecoveryStart(log) // a leftover from an earlier attempt
	src, err := findWinRE()
	if err != nil {
		return nil, err
	}
	st, _ := os.Stat(src)
	if need := uint64(st.Size())*2 + 1<<30; freeBytes(systemDrive()+`\`) < need {
		return nil, fmt.Errorf("the system drive needs at least %d MB free for the restore", need>>20)
	}
	if err := os.MkdirAll(filepath.Join(dir, "mount"), 0o700); err != nil {
		return nil, err
	}
	wim := filepath.Join(dir, "boot.wim")
	log("copying the recovery environment from " + src)
	if err := copyRecoveryFile(src, wim); err != nil {
		return nil, fmt.Errorf("copy the recovery environment: %w", err)
	}
	sdi := filepath.Join(os.Getenv("SystemRoot"), `Boot\DVD\EFI\boot.sdi`)
	if !firmwareUEFI() {
		sdi = filepath.Join(os.Getenv("SystemRoot"), `Boot\DVD\PCAT\boot.sdi`)
	}
	if err := copyRecoveryFile(sdi, filepath.Join(dir, "boot.sdi")); err != nil {
		return nil, fmt.Errorf("copy boot.sdi: %w", err)
	}

	// Network and storage drivers of this computer, for the recovery
	// environment and for the restored Windows (X:\BackupZit\drivers).
	drivers := filepath.Join(dir, "drivers")
	_ = os.MkdirAll(drivers, 0o700)
	if _, err := run(ctx, "pnputil.exe", "/export-driver", "*", drivers); err != nil {
		notes = append(notes, "exporting drivers: "+err.Error())
	}
	kept := keepDrivers(drivers)
	log(fmt.Sprintf("%d network and storage drivers kept", kept))

	mount := filepath.Join(dir, "mount")
	if _, err := run(ctx, "dism.exe", "/English", "/Mount-Image", "/ImageFile:"+wim, "/Index:1", "/MountDir:"+mount); err != nil {
		return nil, fmt.Errorf("open the recovery environment: %w", err)
	}
	commit := false
	defer func() {
		mode := "/Discard"
		if commit {
			mode = "/Commit"
		}
		if _, err := run(context.WithoutCancel(ctx), "dism.exe", "/English", "/Unmount-Image", "/MountDir:"+mount, mode); err != nil && commit {
			notes = append(notes, "closing the recovery environment: "+err.Error())
		}
	}()
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := copyRecoveryFile(self, filepath.Join(mount, `Windows\System32\backupzit-agent.exe`)); err != nil {
		return nil, fmt.Errorf("add the backup program: %w", err)
	}
	bz := filepath.Join(mount, "BackupZit")
	if err := os.MkdirAll(bz, 0o700); err != nil {
		return nil, err
	}
	planJSON, _ := json.Marshal(plan)
	if err := os.WriteFile(filepath.Join(bz, "plan.json"), planJSON, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(mount, `Windows\System32\backupzit-recovery.cmd`), []byte(strings.ReplaceAll(startScript, "\n", "\r\n")), 0o644); err != nil {
		return nil, err
	}
	// Only in the copy: the computer's own recovery environment keeps its menu.
	shell := "[LaunchApps]\r\n%SYSTEMROOT%\\System32\\cmd.exe, /c %SYSTEMROOT%\\System32\\backupzit-recovery.cmd\r\n"
	if err := os.WriteFile(filepath.Join(mount, `Windows\System32\winpeshl.ini`), []byte(shell), 0o644); err != nil {
		return nil, err
	}
	if kept > 0 {
		if _, err := run(ctx, "dism.exe", "/English", "/Image:"+mount, "/Add-Driver", "/Driver:"+drivers, "/Recurse"); err != nil {
			notes = append(notes, "adding drivers to the recovery environment: "+err.Error())
		}
		dst := filepath.Join(bz, "drivers")
		// robocopy reports success with exit codes below 8.
		if _, err := run(ctx, "robocopy.exe", drivers, dst, "/E", "/NFL", "/NDL", "/NJH", "/NJS", "/NP"); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() >= 8 {
				notes = append(notes, "copying drivers for the restored Windows: "+err.Error())
			}
		}
	}
	commit = true
	// The deferred unmount commits; check it before touching the start.
	if _, err := run(ctx, "dism.exe", "/English", "/Unmount-Image", "/MountDir:"+mount, "/Commit"); err != nil {
		commit = false
		return nil, fmt.Errorf("save the recovery environment: %w", err)
	}
	commit = false // already committed

	// A one-time start entry for the copy; the computer's boot menu and its
	// own recovery environment stay as they are.
	out, err := run(ctx, "bcdedit.exe", "/create", "/device")
	if err != nil {
		return nil, err
	}
	options := guidRe.FindString(out)
	state := recoveryState{Options: options}
	saveState := func() { b, _ := json.Marshal(state); _ = os.WriteFile(filepath.Join(dir, "state.json"), b, 0o600) }
	saveState()
	if options == "" {
		return nil, errors.New("creating the ramdisk options failed")
	}
	rel := strings.TrimPrefix(dir, systemDrive())
	for _, args := range [][]string{
		{"/set", options, "ramdisksdidevice", "partition=" + systemDrive()},
		{"/set", options, "ramdisksdipath", rel + `\boot.sdi`},
	} {
		if _, err := run(ctx, "bcdedit.exe", args...); err != nil {
			return nil, err
		}
	}
	out, err = run(ctx, "bcdedit.exe", "/create", "/d", "Unirea Cloud - refacere", "/application", "osloader")
	if err != nil {
		return nil, err
	}
	state.Loader = guidRe.FindString(out)
	saveState()
	if state.Loader == "" {
		return nil, errors.New("creating the start entry failed")
	}
	loader := `\windows\system32\boot\winload.efi`
	if !firmwareUEFI() {
		loader = `\windows\system32\boot\winload.exe`
	}
	device := "ramdisk=[" + systemDrive() + "]" + rel + `\boot.wim,` + options
	for _, args := range [][]string{
		{"/set", state.Loader, "device", device},
		{"/set", state.Loader, "osdevice", device},
		{"/set", state.Loader, "path", loader},
		{"/set", state.Loader, "systemroot", `\windows`},
		{"/set", state.Loader, "winpe", "yes"},
		{"/set", state.Loader, "detecthal", "yes"},
	} {
		if _, err := run(ctx, "bcdedit.exe", args...); err != nil {
			return nil, err
		}
	}
	// BitLocker: the start manager must be able to read the copy on the
	// system drive at the next start (protection resumes after it).
	if bitlockerOn(ctx) {
		if _, err := run(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			"Suspend-BitLocker -MountPoint $env:SystemDrive -RebootCount 1 | Out-Null"); err != nil || bitlockerOn(ctx) {
			return nil, errors.New("BitLocker protection could not be paused for the restart")
		}
		notes = append(notes, "BitLocker protection paused for one restart")
	}
	if _, err := run(ctx, "bcdedit.exe", "/bootsequence", state.Loader); err != nil {
		return nil, err
	}
	log("one-time start into the recovery environment set")
	return notes, nil
}

// cleanupRecoveryStart removes a preparation: the start entries and the copy.
func cleanupRecoveryStart(log func(string)) {
	dir := workDir()
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		var st recoveryState
		if json.Unmarshal(b, &st) == nil {
			for _, id := range []string{st.Loader, st.Options} {
				if id != "" {
					if _, err := run(context.Background(), "bcdedit.exe", "/delete", id, "/f"); err != nil {
						log("removing start entry: " + err.Error())
					}
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "mount", "Windows")); err == nil {
		_, _ = run(context.Background(), "dism.exe", "/English", "/Unmount-Image", "/MountDir:"+filepath.Join(dir, "mount"), "/Discard")
	}
	_ = os.RemoveAll(dir)
}

// CleanupRecoveryStart is called when the agent starts in Windows: a leftover
// preparation means the recovery environment came back without a restore.
func CleanupRecoveryStart(log func(string)) {
	if inRecoveryEnvironment() {
		return
	}
	if _, err := os.Stat(workDir()); err == nil {
		cleanupRecoveryStart(log)
	}
}

func restartForRecovery(log func(string)) {
	if _, err := run(context.Background(), "shutdown.exe", "/r", "/t", "60", "/c",
		"Unirea Cloud reface calculatorul din copie. Calculatorul reporneste in 60 de secunde; salveaza ce ai deschis."); err != nil {
		log(err.Error())
	}
}

func rebootNow(log func(string)) {
	time.Sleep(3 * time.Second) // let the last report leave
	if inRecoveryEnvironment() {
		if _, err := run(context.Background(), "wpeutil.exe", "reboot"); err != nil {
			log(err.Error())
		}
		return
	}
	if _, err := run(context.Background(), "shutdown.exe", "/r", "/t", "5"); err != nil {
		log(err.Error())
	}
}
