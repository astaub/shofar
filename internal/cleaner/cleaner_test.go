package cleaner

import (
	"testing"
	"time"

	"github.com/astaub/shofar/internal/config"
	"github.com/astaub/shofar/internal/proc"
	"github.com/astaub/shofar/internal/worktree"
)

func cfg() config.Config {
	c := config.Default()
	c.MinSessionMinutes = 30
	c.ClaudeIdleHours = 6
	c.StaleAgentMinutes = 120
	return c
}

// inv builds an inventory with the named worktrees marked active/inactive.
func inv(active map[string]bool) *worktree.Inventory {
	var wts []*worktree.Worktree
	for name, isActive := range active {
		wts = append(wts, &worktree.Worktree{Name: name, Path: "/wt/" + name, Active: isActive})
	}
	return &worktree.Inventory{Worktrees: wts}
}

func hasPID(cands []Candidate, pid int) bool {
	for _, c := range cands {
		if c.Proc.PID == pid {
			return true
		}
	}
	return false
}

func TestSelect_OrphanedAgentSelected(t *testing.T) {
	// No TTY AND reparented to launchd (PPID 1) => true orphan, killable.
	procs := []*proc.Proc{
		{PID: 100, PPID: 1, Kind: proc.KindClaude, TTY: "", Elapsed: time.Hour},
	}
	snap := proc.NewSnapshot(procs, 99999)
	cands := Select(cfg(), snap, inv(nil), time.Now())
	if !hasPID(cands, 100) {
		t.Error("orphaned (reparented) claude session should be a candidate")
	}
}

func TestSelect_OrphanReclaimsChildSubtree(t *testing.T) {
	// An ABANDONED detached session: launcher reparented to launchd, and nothing
	// in the tree has a TTY (both no-TTY). It must be flagged exactly once (at the
	// root, not the child too), and its ReclaimBytes must reflect the whole
	// subtree — the 4 MB launcher + its 741 MB child — not just the launcher.
	const mb = 1 << 20
	procs := []*proc.Proc{
		{PID: 100, PPID: 1, Kind: proc.KindClaude, TTY: "", Elapsed: time.Hour, RSSBytes: 4 * mb},     // launcher (orphan root)
		{PID: 101, PPID: 100, Kind: proc.KindClaude, TTY: "", Elapsed: time.Hour, RSSBytes: 741 * mb}, // child (same session)
	}
	snap := proc.NewSnapshot(procs, 99999)
	cands := Select(cfg(), snap, inv(nil), time.Now())

	var roots []Candidate
	for _, c := range cands {
		roots = append(roots, c)
	}
	if len(roots) != 1 {
		t.Fatalf("expected exactly 1 candidate (the session root), got %d: %+v", len(roots), roots)
	}
	got := roots[0]
	if got.Proc.PID != 100 {
		t.Errorf("candidate should be the root launcher pid 100, got pid %d", got.Proc.PID)
	}
	if want := uint64(745 * mb); got.ReclaimBytes != want {
		t.Errorf("ReclaimBytes = %d MB, want %d MB (launcher + child subtree)", got.ReclaimBytes/mb, want/mb)
	}
}

func TestSelect_ProtectedDescendantBlocksKill(t *testing.T) {
	// Orphan launcher (root: no TTY, reparented) whose child is a live session in
	// an ACTIVE worktree. Because Kill signals the whole subtree, selecting the
	// launcher would kill protected work — so it must NOT be a candidate.
	const mb = 1 << 20
	procs := []*proc.Proc{
		{PID: 100, PPID: 1, Kind: proc.KindClaude, TTY: "", Elapsed: time.Hour, RSSBytes: 4 * mb},
		{PID: 101, PPID: 100, Kind: proc.KindClaude, TTY: "", Elapsed: time.Hour, RSSBytes: 700 * mb, WorktreePath: "/wt/active"},
	}
	snap := proc.NewSnapshot(procs, 99999)
	cands := Select(cfg(), snap, inv(map[string]bool{"active": true}), time.Now())
	if hasPID(cands, 100) {
		t.Error("launcher with a child in an active worktree must NOT be selected — kill would take the live child")
	}
}

func TestSelect_NestedDevServersDeduped(t *testing.T) {
	// A dev-server parent and its worker child, both in the same inactive
	// worktree. Only the root is emitted, and its reclaim is the whole subtree —
	// not double-counted across both.
	const mb = 1 << 20
	procs := []*proc.Proc{
		{PID: 200, PPID: 50, Kind: proc.KindDevServer, Elapsed: time.Hour, RSSBytes: 100 * mb, Worktree: "wt", WorktreePath: "/wt/wt"},
		{PID: 201, PPID: 200, Kind: proc.KindDevServer, Elapsed: time.Hour, RSSBytes: 400 * mb, Worktree: "wt", WorktreePath: "/wt/wt"},
	}
	snap := proc.NewSnapshot(procs, 99999)
	cands := Select(cfg(), snap, inv(map[string]bool{"wt": false}), time.Now())
	if len(cands) != 1 {
		t.Fatalf("expected 1 deduped candidate, got %d", len(cands))
	}
	if cands[0].Proc.PID != 200 {
		t.Errorf("expected root pid 200, got %d", cands[0].Proc.PID)
	}
	if want := uint64(500 * mb); cands[0].ReclaimBytes != want {
		t.Errorf("ReclaimBytes = %d MB, want %d MB (parent + worker, counted once)", cands[0].ReclaimBytes/mb, want/mb)
	}
}

func TestSelect_LiveNoTTYAgentProtected(t *testing.T) {
	// No TTY but a LIVE parent (e.g. an editor-spawned agent) => not orphaned,
	// must be protected even though it has no controlling terminal.
	procs := []*proc.Proc{
		{PID: 100, PPID: 4242, Kind: proc.KindClaude, TTY: "", Elapsed: time.Hour},
	}
	snap := proc.NewSnapshot(procs, 99999)
	cands := Select(cfg(), snap, inv(nil), time.Now())
	if hasPID(cands, 100) {
		t.Error("live no-TTY agent (parent alive, PPID != 1) must be protected")
	}
}

func TestSelect_SelfAncestorProtected(t *testing.T) {
	procs := []*proc.Proc{
		{PID: 100, PPID: 50, Kind: proc.KindClaude, TTY: "", Elapsed: time.Hour},
		{PID: 50, PPID: 1, Kind: proc.KindClaude, TTY: "", Elapsed: time.Hour},
	}
	snap := proc.NewSnapshot(procs, 100) // we are pid 100; 100 and 50 are ancestors
	cands := Select(cfg(), snap, inv(nil), time.Now())
	if hasPID(cands, 100) || hasPID(cands, 50) {
		t.Error("self-ancestor sessions must never be selected")
	}
}

func TestSelect_YoungSessionProtected(t *testing.T) {
	procs := []*proc.Proc{
		{PID: 100, Kind: proc.KindClaude, TTY: "", Elapsed: 5 * time.Minute},
	}
	snap := proc.NewSnapshot(procs, 99999)
	cands := Select(cfg(), snap, inv(nil), time.Now())
	if hasPID(cands, 100) {
		t.Error("session younger than MinSessionMinutes must be protected")
	}
}

func TestSelect_ActiveWorktreeProtected(t *testing.T) {
	procs := []*proc.Proc{
		{PID: 100, Kind: proc.KindDevServer, Worktree: "hot", WorktreePath: "/wt/hot", Elapsed: time.Hour},
		{PID: 200, Kind: proc.KindDevServer, Worktree: "cold", WorktreePath: "/wt/cold", Elapsed: time.Hour},
	}
	snap := proc.NewSnapshot(procs, 99999)
	cands := Select(cfg(), snap, inv(map[string]bool{"hot": true, "cold": false}), time.Now())
	if hasPID(cands, 100) {
		t.Error("dev server in active worktree must be protected")
	}
	if !hasPID(cands, 200) {
		t.Error("dev server in inactive worktree should be a candidate")
	}
}

func TestSelect_UnresolvedCwdProtected(t *testing.T) {
	// A relevant process whose cwd could not be resolved must NOT be killed —
	// we can't prove it isn't in an active worktree (fail closed).
	procs := []*proc.Proc{
		{PID: 100, Kind: proc.KindDevServer, CwdUnresolved: true, Elapsed: time.Hour},
	}
	snap := proc.NewSnapshot(procs, 99999)
	cands := Select(cfg(), snap, inv(nil), time.Now())
	if hasPID(cands, 100) {
		t.Error("process with unresolved cwd must be protected (fail closed)")
	}
}

func TestSelect_ProtectPattern(t *testing.T) {
	c := cfg()
	c.ProtectPatterns = []string{"do-not-kill"}
	procs := []*proc.Proc{
		{PID: 100, Kind: proc.KindClaude, TTY: "", Elapsed: time.Hour, Command: "claude --tag do-not-kill"},
	}
	snap := proc.NewSnapshot(procs, 99999)
	cands := Select(c, snap, inv(nil), time.Now())
	if hasPID(cands, 100) {
		t.Error("process matching a protect pattern must be protected")
	}
}

func TestSelect_StaleCursorAgent(t *testing.T) {
	procs := []*proc.Proc{
		{PID: 100, Kind: proc.KindCursorAgent, Elapsed: 3 * time.Hour}, // > 120m => stale
		{PID: 200, Kind: proc.KindCursorAgent, Elapsed: 1 * time.Hour}, // < 120m => keep
	}
	snap := proc.NewSnapshot(procs, 99999)
	cands := Select(cfg(), snap, inv(nil), time.Now())
	if !hasPID(cands, 100) {
		t.Error("3h cursor-agent should be a candidate")
	}
	if hasPID(cands, 200) {
		t.Error("1h cursor-agent should be kept")
	}
}
