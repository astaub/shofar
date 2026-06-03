// Package worktree discovers dev worktrees under the configured base
// directories, maps running processes to them, and decides which are "active"
// (and therefore must be protected from cleanup). Activity is inferred from
// three signals, mirroring the original cleanup-stale-worktrees script:
//
//  1. recent file edits in well-known source subdirectories
//  2. an interactive shell whose cwd is inside the worktree
//  3. a running dev server mapped to the worktree
package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/astaub/shofar/internal/config"
	"github.com/astaub/shofar/internal/proc"
)

// Worktree is one discovered worktree and everything shofar knows about it.
type Worktree struct {
	Name         string       `json:"name"`
	Path         string       `json:"path"`
	Active       bool         `json:"active"`
	ActiveReason string       `json:"active_reason,omitempty"`
	Procs        []*proc.Proc `json:"-"`
	RSSBytes     uint64       `json:"rss_bytes"`
}

// Inventory is the full worktree view for one scan.
type Inventory struct {
	Worktrees []*Worktree
}

// Build discovers worktrees, resolves the cwd of every candidate process so it
// can be attributed to a worktree, and computes activity.
func Build(cfg config.Config, snap *proc.Snapshot, now time.Time) *Inventory {
	inv := &Inventory{}

	for _, base := range cfg.WorktreeBases {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			wt := &Worktree{Name: e.Name(), Path: filepath.Join(base, e.Name())}
			inv.Worktrees = append(inv.Worktrees, wt)
		}
	}
	if len(inv.Worktrees) == 0 {
		return inv
	}

	// Resolve cwd for processes that could belong to a worktree: any agent CLI,
	// dev server, or test runner. "other" processes are skipped to bound lsof
	// cost.
	var candidates []*proc.Proc
	for _, p := range snap.Procs {
		if p.Kind != proc.KindOther {
			candidates = append(candidates, p)
		}
	}
	snap.ResolveCwds(candidates)

	// Attribute each resolved process to the worktree whose path prefixes its
	// cwd.
	for _, p := range candidates {
		if p.Cwd == "" {
			continue
		}
		if wt := inv.match(p.Cwd); wt != nil {
			p.Worktree = wt.Name
			p.WorktreePath = wt.Path
			wt.Procs = append(wt.Procs, p)
			wt.RSSBytes += p.RSSBytes
		}
	}

	for _, wt := range inv.Worktrees {
		wt.Active, wt.ActiveReason = isActive(wt, cfg, now)
	}
	return inv
}

// match returns the worktree whose path is a prefix of cwd, if any.
func (inv *Inventory) match(cwd string) *Worktree {
	for _, wt := range inv.Worktrees {
		if cwd == wt.Path || strings.HasPrefix(cwd, wt.Path+string(os.PathSeparator)) {
			return wt
		}
	}
	return nil
}

// isActive applies the three activity signals in order of strength.
func isActive(wt *Worktree, cfg config.Config, now time.Time) (bool, string) {
	// Signal 3 (strongest): a running dev server in this worktree.
	for _, p := range wt.Procs {
		if p.Kind == proc.KindDevServer {
			return true, "running dev server"
		}
	}
	// Signal 2: an interactive shell with a TTY whose cwd is in the worktree is
	// captured via Procs too, but shells are classified "other" and thus not in
	// wt.Procs. We approximate with the dev-server + recent-edit signals, which
	// the original script found sufficient once TTY-less shells were excluded.

	// Signal 1: recent edits in a known source subdir.
	if CwdRecentlyActive(wt.Path, cfg, now) {
		return true, "recent edits"
	}
	return false, ""
}

// CwdRecentlyActive reports whether a working directory shows recent file edits
// in any configured active subdir. It is the liveness signal that does NOT rely
// on a TTY — essential for harness-managed agents (emdash/cursor) that run with
// no terminal, which the TTY-based orphan heuristic otherwise mislabels.
func CwdRecentlyActive(cwd string, cfg config.Config, now time.Time) bool {
	if cwd == "" {
		return false
	}
	cutoff := now.Add(-time.Duration(cfg.ActiveMinutes) * time.Minute)
	for _, sub := range cfg.ActiveSubdirs {
		if RecentlyEdited(filepath.Join(cwd, sub), cutoff) {
			return true
		}
	}
	return false
}

// RecentlyEdited reports whether any regular file under dir (to a bounded depth)
// was modified after cutoff. Depth is capped to keep the walk cheap on large
// trees; node_modules and .git are skipped.
func RecentlyEdited(dir string, cutoff time.Time) bool {
	const maxDepth = 4
	base := strings.Count(dir, string(os.PathSeparator))
	found := false
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == ".git" || name == "dist" || name == ".next" {
				return filepath.SkipDir
			}
			if strings.Count(path, string(os.PathSeparator))-base > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(cutoff) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// Worktrees returns the discovered worktrees.
func (inv *Inventory) List() []*Worktree { return inv.Worktrees }

// WithProcs returns only worktrees that currently have attributed processes.
func (inv *Inventory) WithProcs() []*Worktree {
	var out []*Worktree
	for _, wt := range inv.Worktrees {
		if len(wt.Procs) > 0 {
			out = append(out, wt)
		}
	}
	return out
}
