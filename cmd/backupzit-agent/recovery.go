package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/imaging"
)

// recoveryConfig is read from \backupzit\recovery.json on any drive (the
// boot USB/ISO), so recovery media can connect without typing.
type recoveryConfig struct {
	Server      string `json:"server"`
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
}

func findRecoveryConfig() (recoveryConfig, string) {
	var candidates []string
	if runtime.GOOS == "windows" {
		for d := 'C'; d <= 'Z'; d++ {
			candidates = append(candidates, string(d)+`:\backupzit\recovery.json`)
		}
	} else {
		// The Linux recovery ISO (live medium) and other mounted media.
		for _, pat := range []string{"/run/live/medium/backupzit/recovery.json", "/mnt/*/backupzit/recovery.json",
			"/media/*/backupzit/recovery.json", "/media/*/*/backupzit/recovery.json"} {
			m, _ := filepath.Glob(pat)
			candidates = append(candidates, m...)
		}
	}
	for _, p := range candidates {
		b, err := os.ReadFile(p) // #nosec G304 -- file named on the command line by the administrator
		if err != nil {
			continue
		}
		var c recoveryConfig
		if json.Unmarshal(b, &c) == nil && c.Server != "" {
			return c, p
		}
	}
	return recoveryConfig{}, ""
}

func prompt(in *bufio.Reader, q string) string {
	fmt.Print(q)
	s, _ := in.ReadString('\n')
	return strings.TrimSpace(s)
}

// cmdRecovery runs a temporary agent from recovery media (WinPE). It
// connects to the server, reports the local disks and executes restore
// runs until the machine is rebooted.
func cmdRecovery(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("recovery", flag.ExitOnError)
	c := recoveryConfig{}
	fs.StringVar(&c.Server, "server", "", "server URL")
	fs.StringVar(&c.Token, "token", "", "recovery/enrollment token")
	fs.StringVar(&c.Fingerprint, "fingerprint", "", "server certificate fingerprint")
	planFile := fs.String("plan", "", "restore plan prepared by the computer's own Windows (Unirea fork)")
	_ = fs.Parse(args)
	if *planFile != "" {
		return recoveryWithPlan(ctx, *planFile)
	}

	fmt.Println("==============================================")
	fmt.Println(" BackupZit recovery environment  " + version)
	fmt.Println("==============================================")
	if c.Server == "" {
		if fc, path := findRecoveryConfig(); path != "" {
			fmt.Println("using settings from", path)
			c = fc
		}
	}
	in := bufio.NewReader(os.Stdin)
	for c.Server == "" {
		c.Server = prompt(in, "Server address (e.g. https://192.168.1.10:8443): ")
	}
	if !strings.HasPrefix(c.Server, "https://") {
		c.Server = "https://" + c.Server
	}
	for c.Token == "" {
		c.Token = prompt(in, "Recovery code (Console > Recovery): ")
	}
	if c.Fingerprint == "" {
		fp, err := agent.FetchFingerprint(ctx, c.Server)
		if err != nil {
			return fmt.Errorf("cannot reach %s: %w", c.Server, err)
		}
		fmt.Println("The server presented this certificate fingerprint:")
		fmt.Println("  " + fp)
		if a := prompt(in, "Does it match the fingerprint shown in the console? [y/N]: "); !strings.EqualFold(a, "y") {
			return errors.New("fingerprint not confirmed")
		}
		c.Fingerprint = fp
	}

	var cfg *agent.Config
	var err error
	for attempt := 1; ; attempt++ {
		cfg, err = agent.EnrollRecovery(ctx, c.Server, c.Token, c.Fingerprint, version)
		if err == nil {
			break
		}
		fmt.Printf("connecting to %s failed: %v\n", c.Server, err)
		if attempt >= 30 || strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "fingerprint") {
			return err
		}
		time.Sleep(5 * time.Second) // network may still be starting
	}
	host, _ := os.Hostname()
	fmt.Printf("connected to %s as %s\n\n", c.Server, host)
	if disks, err := imaging.ListDisks(); err == nil {
		fmt.Println("Local disks:")
		for _, d := range disks {
			fmt.Printf("  Disk %d: %s, %s, %d partitions\n", d.Number, d.Model, humanBytes(d.Size), len(d.Partitions))
		}
	}
	fmt.Println("\nWaiting for a restore started from the console. Keep this window open.")

	a := agent.New(cfg, slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})), version)
	a.VSS = false
	a.InventoryInterval = time.Minute
	a.Run(ctx)
	return nil
}

// recoveryWithPlan is the recovery environment the computer's own Windows
// prepared for one restore plan (Unirea fork): no questions, the settings come
// only from the given file inside the environment, and if the console cannot
// be reached in ten minutes the computer starts its own Windows again,
// untouched (the start into this environment was for one time only).
func recoveryWithPlan(ctx context.Context, file string) error {
	fmt.Println("==============================================")
	fmt.Println(" Unirea Cloud: refacerea calculatorului  " + version)
	fmt.Println("==============================================")
	b, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("restore plan: %w", err)
	}
	var plan api.RecoveryPlan
	if err := json.Unmarshal(b, &plan); err != nil || plan.Server == "" || plan.Token == "" || !strings.HasPrefix(plan.Fingerprint, "SHA256:") {
		return errors.New("the restore plan is damaged")
	}
	deadline := time.Now().Add(10 * time.Minute)
	var cfg *agent.Config
	for {
		cfg, err = agent.EnrollRecovery(ctx, plan.Server, plan.Token, plan.Fingerprint, version)
		if err == nil {
			break
		}
		fmt.Println("Nu ma pot conecta inca la consola de backup:", err)
		if strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "restore") || time.Now().After(deadline) {
			fmt.Println("Nu s-a scris nimic pe disc. Calculatorul porneste din nou Windows-ul lui.")
			return err // the start script restarts the computer
		}
		time.Sleep(10 * time.Second) // the network may still be starting
	}
	fmt.Println("Conectat. Consola verifica discul si porneste refacerea; nu opri calculatorul.")
	a := agent.New(cfg, slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})), version)
	a.VSS = false
	a.InventoryInterval = 30 * time.Second
	// The console answers within a few minutes (restore, or back to Windows);
	// if nothing at all happens, leave rather than wait forever.
	wctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	go func() {
		time.Sleep(30 * time.Minute)
		if !a.Busy() {
			fmt.Println("Consola nu a raspuns. Nu s-a scris nimic; calculatorul porneste din nou Windows-ul lui.")
			cancel()
		}
	}()
	a.Run(wctx)
	return nil
}
