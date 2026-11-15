package main

import (
	"os"
	"path/filepath"
	"strings"
)

// The chosen language is kept per Windows user, next to the tray's other
// files in %LOCALAPPDATA%\BackupZit.
func langFile() string {
	dir := os.Getenv("LOCALAPPDATA")
	if dir == "" {
		dir, _ = os.UserCacheDir()
	}
	return filepath.Join(dir, "BackupZit", "language")
}

// loadLang returns the saved language, Romanian if none was chosen.
//
//lint:ignore U1000 used by the Windows tray
func loadLang() string {
	b, err := os.ReadFile(langFile()) // #nosec G304 -- fixed file in the user's own profile
	if err != nil {
		return langRO
	}
	return normLang(strings.TrimSpace(string(b)))
}

//lint:ignore U1000 used by the Windows tray
func saveLang(l string) {
	p := langFile()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)         // #nosec G703 -- the user's own profile
	_ = os.WriteFile(p, []byte(normLang(l)), 0o600) // #nosec G703 -- the user's own profile
}
