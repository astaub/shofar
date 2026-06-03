// Command shofar is a macOS RAM guard for dev-worktree workflows. It reports
// memory state, answers whether the machine can take on another worktree
// (capacity), cleans up safe-to-kill stale dev processes, and toggles a
// scheduled auto-cleanup agent. All commands accept --json for agent use.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
)

// version is overridden at release time via -ldflags "-X main.version=...".
var version = "0.1.0"

// resolveVersion prefers the release-injected version, then the module version
// stamped by `go install` (Go 1.18+), then the compiled-in default.
func resolveVersion() string {
	if version != "0.1.0" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return version
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "status":
		err = cmdStatus(args)
	case "capacity":
		err = cmdCapacity(args)
	case "clean":
		err = cmdClean(args)
	case "chrome":
		err = cmdChrome(args)
	case "update":
		err = cmdUpdate(args)
	case "cleanup":
		err = cmdCleanup(args)
	case "skill":
		err = cmdSkill(args)
	case "version", "--version", "-v":
		fmt.Println("shofar " + resolveVersion())
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "shofar: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "shofar: "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `shofar 🐏 — macOS RAM guard for dev-worktree workflows

Usage:
  shofar status   [--json]            Memory + worktree + cleanup overview
  shofar capacity [--json]            Can this machine take another worktree?
  shofar clean    [--kill] [--json]   Show (default) or kill safe stale procs
  shofar chrome   [--port N] [--json]  Per-tab memory via Chrome DevTools
  shofar update   [--check]            Rebuild + reinstall from source
  shofar cleanup  on|off|status       Toggle the scheduled auto-cleanup agent
  shofar skill    print|path|install  Install the Agent Skill into a coding agent
  shofar version
`)
}

// fmtBytes renders a byte count as a compact human string (e.g. "3.4 GB").
func fmtBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
