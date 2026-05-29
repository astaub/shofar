package main

import (
	"fmt"
	"os"

	"github.com/astaub/shofar/internal/config"
	"github.com/astaub/shofar/internal/launchd"
)

const cleanupInterval = 3600 // seconds

func cmdCleanup(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: shofar cleanup on|off|status")
	}
	switch args[0] {
	case "on":
		return cleanupOn()
	case "off":
		return cleanupOff()
	case "status":
		return cleanupStatus()
	default:
		return fmt.Errorf("usage: shofar cleanup on|off|status")
	}
}

func cleanupOn() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own path: %w", err)
	}
	if err := launchd.Install(bin, cleanupInterval); err != nil {
		return err
	}
	cfg.CleanupEnabled = true
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("Scheduled cleanup ON — `%s clean --kill` runs hourly and at login.\n", bin)
	return nil
}

func cleanupOff() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := launchd.Uninstall(); err != nil {
		return err
	}
	cfg.CleanupEnabled = false
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Println("Scheduled cleanup OFF — the launchd agent has been removed.")
	return nil
}

func cleanupStatus() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	loaded := launchd.Status()
	fmt.Printf("config:  cleanup_enabled = %v\n", cfg.CleanupEnabled)
	fmt.Printf("launchd: agent loaded   = %v\n", loaded)
	fmt.Printf("plist:   %s\n", launchd.PlistPath())
	if cfg.CleanupEnabled != loaded {
		fmt.Println("\nNote: config and launchd disagree. Run `shofar cleanup on` or `off` to reconcile.")
	}
	return nil
}
