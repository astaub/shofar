package main

import (
	"testing"

	"github.com/astaub/shofar/internal/proc"
	"github.com/astaub/shofar/internal/worktree"
)

// TestDeriveRole pins the role boundary: a session root is an agent, everything
// in its tree is agent-child (including an inner agent process), a process
// reparented to launchd with no agent above it is an orphan, and a process
// parented to a live non-agent is "other".
func TestDeriveRole(t *testing.T) {
	procs := []*proc.Proc{
		{PID: 50, PPID: 400, Kind: proc.KindOther},     // a shell; parent 400 not in snap
		{PID: 100, PPID: 50, Kind: proc.KindClaude},    // session root → agent
		{PID: 101, PPID: 100, Kind: proc.KindClaude},   // inner agent → agent-child
		{PID: 102, PPID: 100, Kind: proc.KindOther},    // shell under agent → agent-child
		{PID: 103, PPID: 102, Kind: proc.KindOther},    // grandchild under agent → agent-child
		{PID: 200, PPID: 1, Kind: proc.KindDevServer},  // reparented, no agent above → orphan
		{PID: 300, PPID: 50, Kind: proc.KindDevServer}, // under live shell, no agent → other
	}
	snap := proc.NewSnapshot(procs, 99999)
	want := map[int]procRole{
		50:  roleOther,
		100: roleAgent,
		101: roleAgentChild,
		102: roleAgentChild,
		103: roleAgentChild,
		200: roleOrphan,
		300: roleOther,
	}
	for _, p := range procs {
		got := deriveRole(snap, p)
		if got != want[p.PID] {
			t.Errorf("deriveRole(pid %d) = %q, want %q", p.PID, got, want[p.PID])
		}
	}
}

// TestResolveWorktree pins the name-collision footgun: a name shared across bases
// must NOT silently resolve to one tree; a full path always wins.
func TestResolveWorktree(t *testing.T) {
	inv := &worktree.Inventory{Worktrees: []*worktree.Worktree{
		{Name: "alpha", Path: "/base1/alpha"},
		{Name: "dup", Path: "/base1/dup"},
		{Name: "dup", Path: "/base2/dup"},
	}}

	if wt, amb := resolveWorktree(inv, "alpha"); wt == nil || wt.Path != "/base1/alpha" || amb != nil {
		t.Errorf("unique name: got wt=%v amb=%v", wt, amb)
	}
	if wt, amb := resolveWorktree(inv, "/base2/dup"); wt == nil || wt.Path != "/base2/dup" || amb != nil {
		t.Errorf("path match: got wt=%v amb=%v", wt, amb)
	}
	if wt, amb := resolveWorktree(inv, "dup"); wt != nil || len(amb) != 2 {
		t.Errorf("ambiguous name: want nil + 2 candidates, got wt=%v amb=%d", wt, len(amb))
	}
	if wt, amb := resolveWorktree(inv, "ghost"); wt != nil || len(amb) != 0 {
		t.Errorf("not found: want nil + 0, got wt=%v amb=%d", wt, len(amb))
	}
}

// TestWorktreeProcs verifies a worktree's attributed process pulls in its whole
// subtree (so the agent's shell/children show), sorted by RSS. No PPID==1 member
// here, so the orphan-cwd path (which would lsof) stays inert.
func TestWorktreeProcs(t *testing.T) {
	shell := &proc.Proc{PID: 50, PPID: 400, Kind: proc.KindOther}
	agent := &proc.Proc{PID: 100, PPID: 50, Kind: proc.KindClaude, RSSBytes: 600 << 20}
	child := &proc.Proc{PID: 102, PPID: 100, Kind: proc.KindOther, RSSBytes: 5 << 20}
	snap := proc.NewSnapshot([]*proc.Proc{shell, agent, child}, 99999)
	wt := &worktree.Worktree{Name: "x", Path: "/x", Procs: []*proc.Proc{agent}}

	rows := worktreeProcs(snap, wt)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (agent + child), got %d", len(rows))
	}
	if rows[0].PID != 100 {
		t.Errorf("want heaviest (agent pid 100) first, got pid %d", rows[0].PID)
	}
	if rows[1].PID != 102 {
		t.Errorf("want child pid 102 second, got pid %d", rows[1].PID)
	}
}
