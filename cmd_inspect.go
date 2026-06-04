package main

// cmd_inspect.go is READ-ONLY by construction: it imports proc, worktree, and
// config but deliberately NOT internal/cleaner. It never selects or signals a
// process — it only describes what is running inside a worktree so a human can
// decide. The per-process "role" it reports (agent / agent-child / orphan /
// other) is a descriptive hint, NOT a kill verdict. Keep it that way: the moment
// this file needs internal/cleaner, the read-only contract is broken.

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/astaub/shofar/internal/proc"
	"github.com/astaub/shofar/internal/worktree"
)

// procRole is a descriptive classification of a process inside a worktree. It
// answers "is this the live agent, something the agent spawned, or a leftover
// orphan?" — the distinction the collapsed status row hides. It is a hint for a
// human, never an eligibility decision.
type procRole string

const (
	roleAgent      procRole = "agent"       // a claude/codex/cursor session root — live, irreducible
	roleAgentChild procRole = "agent-child" // anything inside a live agent's process tree
	roleOrphan     procRole = "orphan"      // reparented to launchd (PPID==1), not under any agent
	roleOther      procRole = "other"       // parented to some live non-agent process
)

func isAgentKind(k proc.Kind) bool {
	return k == proc.KindClaude || k == proc.KindCodex || k == proc.KindCursorAgent
}

// hasAgentAncestor reports whether any strict ancestor of p is an agent process.
// It walks the PPID chain via the snapshot, guarding against cycles and stopping
// at launchd (PID 1).
func hasAgentAncestor(snap *proc.Snapshot, p *proc.Proc) bool {
	seen := map[int]bool{}
	ppid := p.PPID
	for ppid > 1 && !seen[ppid] {
		seen[ppid] = true
		parent, ok := snap.LookupPID(ppid)
		if !ok {
			return false
		}
		if isAgentKind(parent.Kind) {
			return true
		}
		ppid = parent.PPID
	}
	return false
}

// deriveRole classifies one process. The order matters: an agent process whose
// parent is also an agent is the inner process of one session (agent-child), not
// a second session; everything under a live agent is agent-child; a process
// reparented to launchd with no agent above it is an orphan; anything else is
// parented to a live non-agent (e.g. a shell) and is "other".
func deriveRole(snap *proc.Snapshot, p *proc.Proc) procRole {
	if isAgentKind(p.Kind) {
		if parent, ok := snap.LookupPID(p.PPID); ok && isAgentKind(parent.Kind) {
			return roleAgentChild
		}
		return roleAgent
	}
	if hasAgentAncestor(snap, p) {
		return roleAgentChild
	}
	if p.PPID == 1 {
		return roleOrphan
	}
	return roleOther
}

// looksSystem is a cheap filter to avoid an lsof on obvious macOS system daemons
// (which are all reparented to launchd) when hunting for orphans in a worktree.
func looksSystem(command string) bool {
	for _, pre := range []string{"/usr/", "/System/", "/sbin/", "/Library/", "/Applications/"} {
		if strings.HasPrefix(command, pre) {
			return true
		}
	}
	return false
}

func underPath(cwd, base string) bool {
	return cwd == base || strings.HasPrefix(cwd, base+string(os.PathSeparator))
}

// worktreeProcs returns every process to show for a worktree: each attributed
// process and its full subtree (so KindOther children like the agent's shell and
// caffeinate appear), plus any reparented orphan whose cwd resolves under the
// worktree (so a leaked dev-server/node the attribution pass skipped still shows).
// cwd resolution is bounded to reparented, non-system candidates.
func worktreeProcs(snap *proc.Snapshot, wt *worktree.Worktree) []*proc.Proc {
	seen := map[int]bool{}
	var rows []*proc.Proc
	add := func(p *proc.Proc) {
		if !seen[p.PID] {
			seen[p.PID] = true
			rows = append(rows, p)
		}
	}
	for _, root := range wt.Procs {
		for _, m := range snap.Subtree(root.PID) {
			// A subtree can cross worktree boundaries (a launcher in one worktree
			// whose child agent runs in another). Show a member that's attributed
			// elsewhere under ITS worktree, not this one — otherwise the same PID
			// appears in two breakdowns. Unattributed members (KindOther children
			// like the agent's shell/caffeinate) have no WorktreePath and stay.
			if m.WorktreePath != "" && m.WorktreePath != wt.Path {
				continue
			}
			add(m)
		}
	}
	var orphanCands []*proc.Proc
	for _, p := range snap.Procs {
		if p.PPID == 1 && !seen[p.PID] && p.Cwd == "" && !looksSystem(p.Command) {
			orphanCands = append(orphanCands, p)
		}
	}
	snap.ResolveCwds(orphanCands)
	for _, p := range orphanCands {
		if p.Cwd != "" && underPath(p.Cwd, wt.Path) {
			for _, m := range snap.Subtree(p.PID) {
				add(m)
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RSSBytes > rows[j].RSSBytes })
	return rows
}

// resolveWorktree matches arg against the inventory by exact path first, then by
// name. It returns (wt, nil) on a unique match, (nil, candidates) when a name is
// ambiguous across bases (>1), and (nil, nil) when nothing matches. Path-keying
// mirrors the cleaner, which keys active worktrees by path precisely because
// names collide across configured bases.
func resolveWorktree(inv *worktree.Inventory, arg string) (*worktree.Worktree, []*worktree.Worktree) {
	for _, wt := range inv.List() {
		if wt.Path == arg {
			return wt, nil
		}
	}
	var byName []*worktree.Worktree
	for _, wt := range inv.List() {
		if wt.Name == arg {
			byName = append(byName, wt)
		}
	}
	if len(byName) == 1 {
		return byName[0], nil
	}
	return nil, byName
}

// inspectProc is the serialized per-process row. Raw facts first (ppid, tty,
// kind) so a caller can draw its own conclusion; role is a labeled hint.
type inspectProc struct {
	PID            int       `json:"pid"`
	PPID           int       `json:"ppid"`
	Role           procRole  `json:"role"`
	Kind           proc.Kind `json:"kind"`
	RSSBytes       uint64    `json:"rss_bytes"`
	TTY            string    `json:"tty"`
	TTYIdleSeconds *int64    `json:"tty_idle_seconds,omitempty"`
	ElapsedSeconds int64     `json:"elapsed_seconds"`
	Cwd            string    `json:"cwd,omitempty"`
	Command        string    `json:"command"`
}

type inspectOut struct {
	Worktree struct {
		Name         string `json:"name"`
		Path         string `json:"path"`
		Active       bool   `json:"active"`
		ActiveReason string `json:"active_reason,omitempty"`
		RSSBytes     uint64 `json:"rss_bytes"`
	} `json:"worktree"`
	Processes []inspectProc `json:"processes"`
}

func buildInspect(snap *proc.Snapshot, wt *worktree.Worktree, now time.Time) inspectOut {
	var out inspectOut
	out.Worktree.Name = wt.Name
	out.Worktree.Path = wt.Path
	out.Worktree.Active = wt.Active
	out.Worktree.ActiveReason = wt.ActiveReason
	out.Worktree.RSSBytes = wt.RSSBytes
	for _, p := range worktreeProcs(snap, wt) {
		ip := inspectProc{
			PID:            p.PID,
			PPID:           p.PPID,
			Role:           deriveRole(snap, p),
			Kind:           p.Kind,
			RSSBytes:       p.RSSBytes,
			TTY:            p.TTY,
			ElapsedSeconds: int64(p.Elapsed.Seconds()),
			Cwd:            p.Cwd,
			Command:        p.Command,
		}
		if idle, ok := proc.TTYIdle(p, now); ok {
			if idle < 0 { // tty touched after the scan snapshot; clock-skew, treat as fresh
				idle = 0
			}
			s := int64(idle.Seconds())
			ip.TTYIdleSeconds = &s
		}
		out.Processes = append(out.Processes, ip)
	}
	return out
}

func cmdInspect(args []string) error {
	jsonOut := hasFlag(args, "--json")
	var target string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			target = a
			break
		}
	}
	if target == "" {
		return fmt.Errorf("usage: shofar inspect <worktree-name-or-path> [--json]")
	}

	s, err := newScan()
	if err != nil {
		return err
	}

	wt, ambiguous := resolveWorktree(s.inv, target)
	if wt == nil {
		if len(ambiguous) > 1 {
			var b strings.Builder
			fmt.Fprintf(&b, "worktree name %q is ambiguous across bases; pass a full path:", target)
			for _, c := range ambiguous {
				fmt.Fprintf(&b, "\n  %s", c.Path)
			}
			return fmt.Errorf("%s", b.String())
		}
		return fmt.Errorf("no worktree matching %q (try `shofar status` to list worktrees)", target)
	}

	out := buildInspect(s.snap, wt, s.now)

	if jsonOut {
		return emitJSON(out)
	}

	colorOn = !hasFlag(args, "--no-color") &&
		(hasFlag(args, "--color") || os.Getenv("CLICOLOR_FORCE") != "" || isTerminal(os.Stdout))

	active := "idle"
	if wt.Active {
		active = "active"
		if wt.ActiveReason != "" {
			active += " — " + wt.ActiveReason
		}
	}
	fmt.Printf("%s  %s  ·  %s\n", bold(wt.Name), dim(wt.Path), active)
	fmt.Printf("%s across %d process(es)\n\n", fmtBytes(out.Worktree.RSSBytes), len(out.Processes))
	printProcTable(out.Processes, s.now, 0)
	fmt.Println(dim("\nread-only: inspect never kills. Use `shofar clean` for safe-to-kill stale procs."))
	return nil
}

// printProcTable renders inspectProc rows as an aligned table. topN caps the rows
// (0 = all); when capped, a trailing line notes how many were hidden.
func printProcTable(rows []inspectProc, now time.Time, topN int) {
	const cmdW = 38
	hdr := func(role, pid, kind, ram, tty, age, cmd string) {
		fmt.Printf("  %s %7s  %-11s %9s  %8s  %6s  %s\n",
			padRight(role, 11), pid, kind, ram, tty, age, cmd)
	}
	hdr("ROLE", "PID", "KIND", "RAM", "TTY", "AGE", "COMMAND")
	hdr(strings.Repeat("─", 11), "───────", "───────────", "─────────", "────────", "──────", strings.Repeat("─", cmdW))
	shown := rows
	hidden := 0
	if topN > 0 && len(rows) > topN {
		shown = rows[:topN]
		hidden = len(rows) - topN
	}
	for _, r := range shown {
		tty := "-"
		if r.TTYIdleSeconds != nil {
			tty = "idle " + shortDur(time.Duration(*r.TTYIdleSeconds)*time.Second)
		}
		fmt.Printf("  %s %7d  %-11s %9s  %8s  %6s  %s\n",
			colorRole(r.Role), r.PID, string(r.Kind), fmtBytes(r.RSSBytes),
			tty, shortDur(time.Duration(r.ElapsedSeconds)*time.Second), truncRunes(r.Command, cmdW))
	}
	if hidden > 0 {
		fmt.Printf("  %s\n", dim(fmt.Sprintf("… +%d smaller process(es) (use --all)", hidden)))
	}
}

// colorRole tints the role cell: agent cyan (leave it), orphan yellow
// (reclaimable), agent-child dim, other plain. Padded to the column width.
func colorRole(r procRole) string {
	s := padRight(string(r), 11)
	switch r {
	case roleAgent:
		return cyan(s)
	case roleOrphan:
		return yellow(s)
	case roleAgentChild:
		return dim(s)
	default:
		return s
	}
}

// shortDur renders a duration compactly: "3h", "12m", "45s".
func shortDur(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
