package main

import (
	"errors"
	"io"
	"os"
	"strings"
)

func validateTokenFlags(name, role string, days int) error {
	if strings.TrimSpace(name) == "" || len(strings.TrimSpace(name)) > 100 {
		return errors.New("give the token a name (at most 100 bytes)")
	}
	switch role {
	case "admin", "operator", "restore", "viewer":
	default:
		return errors.New("role must be admin, operator, restore or viewer")
	}
	if days < 0 || days > 3650 {
		return errors.New("expires-days must be between 0 and 3650")
	}
	return nil
}

func readAdminPasswordFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", err
	}
	if len(b) > 4096 {
		return "", errors.New("admin password file is too large")
	}
	pw := strings.TrimRight(string(b), "\r\n")
	if len(pw) < 8 || len(pw) > 72 || strings.ContainsAny(pw, "\x00\r\n") {
		return "", errors.New("admin password file must contain one password of 8 to 72 bytes")
	}
	return pw, nil
}
