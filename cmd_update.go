package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
)

// cmdUpdate rebuilds shofar from its source repo and installs over the running
// binary, so improvements land without remembering the go build incantation or
// cd-ing into the repo. `--check` reports staleness without building.
//
// Discovery of new worktrees/repos is a RUNTIME concern — every `status` rescans
// — so "learning about new repos" needs no rebuild. This command is for picking
// up new shofar *code*.
func cmdUpdate(args []string) error {
	src, err := findShofarSource()
	if err != nil {
		return err
	}
	dest, derr := os.Executable()
	if derr != nil {
		return fmt.Errorf("locate installed binary: %w", derr)
	}
	srcRev := gitRevision(src)
	binRev := installedRevision()

	if hasFlag(args, "--check") {
		fmt.Printf("source:    %s @ %s\n", src, srcRev)
		fmt.Printf("installed: %s @ %s\n", dest, orUnknown(binRev))
		if binRev != "" && binRev == srcRev {
			fmt.Println("up to date.")
		} else {
			fmt.Println("behind source — run `shofar update` to rebuild.")
		}
		return nil
	}

	fmt.Printf("Building shofar from %s (%s) → %s\n", src, srcRev, dest)
	// Stamp the revision so a future `shofar update --check` can compare reliably
	// (Go's own VCS stamping is absent when building from a worktree).
	cmd := exec.Command("go", "build", "-ldflags", "-X main.builtRevision="+srcRev, "-o", dest, ".")
	cmd.Dir = src
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build failed (is Go installed?): %w", err)
	}
	fmt.Printf("Installed shofar @ %s\n", srcRev)
	return nil
}

// findShofarSource locates the shofar source checkout: $SHOFAR_SRC, the current
// directory if it's the repo, then ~/code/shofar.
func findShofarSource() (string, error) {
	var candidates []string
	if s := os.Getenv("SHOFAR_SRC"); s != "" {
		candidates = append(candidates, s)
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, cwd)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "code", "shofar"))
	}
	for _, c := range candidates {
		if isShofarRepo(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("shofar source not found — set SHOFAR_SRC=<path> or clone to ~/code/shofar")
}

func isShofarRepo(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	return err == nil && strings.Contains(string(data), "module github.com/astaub/shofar")
}

// gitRevision returns the short HEAD of dir, suffixed +dirty when the tree has
// uncommitted changes.
func gitRevision(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	rev := strings.TrimSpace(string(out))
	if st, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); strings.TrimSpace(string(st)) != "" {
		rev += "+dirty"
	}
	return rev
}

// builtRevision is injected by `shofar update` via -ldflags. Empty for binaries
// built any other way (then we fall back to Go's VCS stamp).
var builtRevision string

// installedRevision reports the source revision this binary was built from —
// the ldflags stamp if present, else Go's VCS build info.
func installedRevision() string {
	if builtRevision != "" {
		return builtRevision
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) > 7 {
				rev = s.Value[:7]
			} else {
				rev = s.Value
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if dirty && rev != "" {
		rev += "+dirty"
	}
	return rev
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown (no VCS stamp)"
	}
	return s
}
