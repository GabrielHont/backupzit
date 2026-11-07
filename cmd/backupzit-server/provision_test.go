package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmptyTokenNameCLI(t *testing.T) {
	args, flags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args = args; flag.CommandLine = flags })
	for _, name := range []string{"", " "} {
		flag.CommandLine = flag.NewFlagSet("backupzit-server", flag.ContinueOnError)
		data := filepath.Join(t.TempDir(), "must-not-start")
		os.Args = []string{"backupzit-server", "--create-api-token", name, "--data-dir", data, "--db", ""}
		if err := run(); err == nil || !strings.Contains(err.Error(), "name") {
			t.Fatalf("empty token name entered normal startup: %v", err)
		}
		if _, err := os.Stat(data); !os.IsNotExist(err) {
			t.Fatal("invalid provisioning command wrote a data directory")
		}
	}
}

func TestProvisionTokenFlags(t *testing.T) {
	for _, role := range []string{"admin", "operator", "restore", "viewer"} {
		if err := validateTokenFlags("portal", role, 30); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, role string
		days       int
	}{{" ", "admin", 0}, {strings.Repeat("a", 101), "admin", 0}, {"portal", "root", 0}, {"portal", "admin", -1}, {"portal", "admin", 3651}} {
		if err := validateTokenFlags(tc.name, tc.role, tc.days); err == nil {
			t.Errorf("accepted %+v", tc)
		}
	}
}

func TestAdminPasswordFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "admin-password")
	for _, input := range []string{"a good password\n", "a good password\r\n", "  spaces stay  \n"} {
		if err := os.WriteFile(p, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		pw, err := readAdminPasswordFile(p)
		if err != nil || pw != strings.TrimRight(input, "\r\n") {
			t.Fatalf("password file: %q %v", pw, err)
		}
	}
	for _, input := range []string{"", "short\n", "first-line\nsecond-line", strings.Repeat("a", 73), strings.Repeat("a", 5000), "nul\x00password"} {
		if err := os.WriteFile(p, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readAdminPasswordFile(p); err == nil {
			t.Errorf("accepted invalid password file of %d bytes", len(input))
		}
	}
}
