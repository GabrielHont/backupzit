package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
