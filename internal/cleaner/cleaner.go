// Package cleaner selects safe-to-kill processes and (optionally) terminates
// them. The selection rules are deliberately conservative — the cost of a false
// positive (killing live work) is far higher than a missed stale process:
//
//   - never kill a process in our own ancestor chain (self-protection)
//   - never kill a process attributed to an ACTIVE worktree
//   - never kill a session younger than MinSessionMinutes
//   - never kill a process whose command matches a protect pattern
//   - claude/codex: eligible only when their TTY has been idle past the window
//     (or they have no TTY at all, i.e. orphaned)
//   - cursor-agent / dev-server / test-runner: eligible when stale by runtime
//     and not in an active worktree
package cleaner

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/astaub/shofar/internal/config"
	"github.com/astaub/shofar/internal/proc"
	"github.com/astaub/shofar/internal/worktree"
)

// Candidate is a process selected for cleanup, with a human-readable reason.
type Candidate struct {
	Proc   *proc.Proc `json:"proc"`
	Reason string     `json:"reason"`
}

// Select returns the cleanup candidates for the current scan. It mutates
// nothing; killing is a separate explicit step.
func Select(cfg config.Config, snap *proc.Snapshot, inv *worktree.Inventory, now time.Time) []Candidate {
	// Key the active set by PATH, not name: worktree names can collide across
	// configured bases, and protecting the wrong one would expose a live tree.
	activeWorktrees := map[string]bool{}
	for _, wt := range inv.List() {
		if wt.Active {
			activeWorktrees[wt.Path] = true
		}
	}

	minSession := time.Duration(cfg.MinSessionMinutes) * time.Minute
	claudeIdle := time.Duration(cfg.ClaudeIdleHours) * time.Hour
	staleAgent := time.Duration(cfg.StaleAgentMinutes) * time.Minute

	var out []Candidate
	for _, p := range snap.Procs {
		// Hard exclusions first.
		if snap.IsSelfAncestor(p.PID) {
			continue
		}
		if p.WorktreePath != "" && activeWorktrees[p.WorktreePath] {
			continue
		}
		// Fail closed: a process whose working directory could not be resolved
		// can't be placed in (or cleared of) an active worktree, so we can't
		// prove it's safe to kill.
		if p.CwdUnresolved {
			continue
		}
		if matchesProtect(p.Command, cfg.ProtectPatterns) {
			continue
		}
		if p.Elapsed < minSession {
			continue
		}

		switch p.Kind {
		case proc.KindClaude, proc.KindCodex:
			idle, hasTTY := proc.TTYIdle(p, now)
			switch {
			case hasTTY && idle >= claudeIdle:
				out = append(out, Candidate{p, string(p.Kind) + " session idle " + round(idle)})
			case !hasTTY && isReparented(p):
				// No controlling terminal AND reparented to launchd (PPID 1) =>
				// the session's parent died, so it's a true orphan. A live
				// no-TTY agent (e.g. an editor- or IDE-spawned session) still
				// has its real parent, so PPID != 1 and we leave it alone.
				out = append(out, Candidate{p, "orphaned " + string(p.Kind) + " session (no TTY, reparented to launchd)"})
			}
		case proc.KindCursorAgent:
			if p.Elapsed >= staleAgent {
				out = append(out, Candidate{p, "stale cursor-agent running " + round(p.Elapsed)})
			}
		case proc.KindDevServer, proc.KindTestRunner:
			// Only eligible when attributed to a known but inactive worktree:
			// an unattributed dev server may be the user's primary one.
			if p.Worktree != "" {
				out = append(out, Candidate{p, string(p.Kind) + " in inactive worktree " + p.Worktree})
			}
		}
	}
	return out
}

// Kill sends SIGTERM to each candidate. Returns the PIDs successfully signaled,
// the PIDs skipped because they no longer match the scanned process (PID reuse
// or already exited), and a map of PID->error for failures.
//
// Each candidate is re-verified against the live process table immediately
// before signaling: between the scan and the kill a candidate could have exited
// and had its PID recycled to an unrelated process, which we must not signal.
func Kill(cands []Candidate) (killed, skipped []int, errs map[int]error) {
	errs = map[int]error{}
	for _, c := range cands {
		if !proc.Verify(c.Proc) {
			skipped = append(skipped, c.Proc.PID)
			continue
		}
		p, err := os.FindProcess(c.Proc.PID)
		if err != nil {
			errs[c.Proc.PID] = err
			continue
		}
		if err := p.Signal(syscall.SIGTERM); err != nil {
			errs[c.Proc.PID] = err
			continue
		}
		killed = append(killed, c.Proc.PID)
	}
	return killed, skipped, errs
}

// isReparented reports whether a process has been reparented to launchd (PID 1),
// the macOS signal that its original parent has exited.
func isReparented(p *proc.Proc) bool { return p.PPID == 1 }

func matchesProtect(command string, patterns []string) bool {
	for _, pat := range patterns {
		if pat != "" && strings.Contains(command, pat) {
			return true
		}
	}
	return false
}

// round renders a duration to a compact "Xh" / "Xm" string for reasons.
func round(d time.Duration) string {
	if d >= time.Hour {
		return strconv.Itoa(int(d.Hours())) + "h"
	}
	return strconv.Itoa(int(d.Minutes())) + "m"
}
