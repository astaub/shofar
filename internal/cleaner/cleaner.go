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
	// ReclaimBytes is the memory a kill actually frees: the process plus every
	// descendant that dies with it. For an orphaned launcher this is far larger
	// than the launcher's own RSS — it's the child agent it spawned.
	ReclaimBytes uint64 `json:"reclaim_bytes"`
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
	add := func(p *proc.Proc, reason string) {
		out = append(out, Candidate{Proc: p, Reason: reason, ReclaimBytes: snap.SubtreeRSS(p.PID)})
	}
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
			// Only judge the session ROOT. A claude/codex whose parent is itself
			// a claude/codex is the inner process of one session (e.g. the child
			// of a `tmux new-session -d ... claude` launcher); the root's subtree
			// walk already accounts for it, so emitting it again would
			// double-count the reclaim and risk a redundant kill.
			if isChildOfSession(snap, p) {
				continue
			}
			// Liveness is a property of the whole subtree, not this process's own
			// TTY: a detached session's launcher has no TTY but its child does.
			// Only abandoned when NOTHING in the tree has a recently-active
			// terminal.
			idle, hasTTY := snap.SubtreeMinTTYIdle(p.PID, now)
			switch {
			case hasTTY && idle >= claudeIdle:
				add(p, string(p.Kind)+" session idle "+round(idle))
			case !hasTTY && isReparented(p):
				// No TTY anywhere in the tree AND reparented to launchd (PPID 1)
				// => the session's parent died and no terminal is attached, so
				// it's a true orphan. A live no-TTY agent (editor/IDE-spawned)
				// still has its real parent (PPID != 1) and is left alone.
				add(p, "orphaned "+string(p.Kind)+" session (no TTY in tree, reparented to launchd)")
			}
		case proc.KindCursorAgent:
			if p.Elapsed >= staleAgent {
				add(p, "stale cursor-agent running "+round(p.Elapsed))
			}
		case proc.KindDevServer, proc.KindTestRunner:
			// Only eligible when attributed to a known but inactive worktree:
			// an unattributed dev server may be the user's primary one.
			if p.Worktree != "" {
				add(p, string(p.Kind)+" in inactive worktree "+p.Worktree)
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

// isChildOfSession reports whether p's parent is itself an agent process of the
// same kind family — i.e. p is the inner process of a single session, not its
// root. We classify a session by its root so a launcher + its child count once.
func isChildOfSession(snap *proc.Snapshot, p *proc.Proc) bool {
	parent, ok := snap.LookupPID(p.PPID)
	if !ok {
		return false
	}
	return parent.Kind == proc.KindClaude || parent.Kind == proc.KindCodex
}

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
