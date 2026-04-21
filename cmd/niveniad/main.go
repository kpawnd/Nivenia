package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"nivenia/internal/config"
	"nivenia/internal/engine"
	"nivenia/internal/platform"
)

func waitForManagedVolume(path string) error {
	// Run as early as possible, but wait for the configured managed volume/path.
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("managed_root is empty")
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for managed_root: %s", path)
}

func consoleUser() (string, error) {
	out, err := exec.Command("stat", "-f%Su", "/dev/console").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func waitForLoginWindow() error {
	// Ensure restore happens before any interactive user session starts.
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		user, err := consoleUser()
		if err == nil {
			if user == "" || user == "root" || user == "loginwindow" {
				return nil
			}
			return fmt.Errorf("interactive console user detected: %s", user)
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for loginwindow console user")
}

func main() {
	policyPath := flag.String("policy", "/etc/nivenia/policy.json", "policy file path")
	requireLoginWindow := flag.Bool("require-loginwindow", false, "abort if an interactive user is at the console (set in plist for boot-time use)")
	flag.Parse()

	if err := platform.EnsureSupportedMacOS(); err != nil {
		fmt.Fprintf(os.Stderr, "niveniad: %v\n", err)
		os.Exit(1)
	}

	p, err := config.Load(*policyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "niveniad: %v\n", err)
		os.Exit(1)
	}

	if err := waitForManagedVolume(p.ManagedRoot); err != nil {
		fmt.Fprintf(os.Stderr, "niveniad preboot check failed: %v\n", err)
		os.Exit(1)
	}
	if *requireLoginWindow {
		if err := waitForLoginWindow(); err != nil {
			fmt.Fprintf(os.Stderr, "niveniad preboot check failed: %v\n", err)
			os.Exit(1)
		}
	}

	e := engine.New(p)
	if err := e.RunBootRestore(); err != nil {
		fmt.Fprintf(os.Stderr, "niveniad restore failed: %v\n", err)
		os.Exit(2)
	}
}
