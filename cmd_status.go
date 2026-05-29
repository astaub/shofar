package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/astaub/shofar/internal/capacity"
	"github.com/astaub/shofar/internal/cleaner"
	"github.com/astaub/shofar/internal/config"
	"github.com/astaub/shofar/internal/proc"
	"github.com/astaub/shofar/internal/sysinfo"
	"github.com/astaub/shofar/internal/worktree"
)

const shofarArt = `
:+....
#+..
*#-.
:#%=.     .:=+=:..
.:*%#+-:..-+*+=-..
...-*%%%#**+++-:..
        ...:---+*==:.
               .=*==-...
             .-*--=-..            ...
             .-+-=+=..          ..-=-:.
           .:==+**+:.......::-=+*#*:
            ..:=+++===----====+++#*-
               ..-=+=-:::--=-------:
                  .::--------:::....
`

// scan bundles the work shared by status/capacity/clean.
type scan struct {
	cfg  config.Config
	mem  sysinfo.Memory
	snap *proc.Snapshot
	inv  *worktree.Inventory
	now  time.Time
}

func newScan() (*scan, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	mem, err := sysinfo.Read()
	if err != nil {
		return nil, fmt.Errorf("read memory: %w", err)
	}
	snap, err := proc.Scan()
	if err != nil {
		return nil, fmt.Errorf("scan processes: %w", err)
	}
	now := time.Now()
	inv := worktree.Build(cfg, snap, now)
	return &scan{cfg: cfg, mem: mem, snap: snap, inv: inv, now: now}, nil
}

func cmdStatus(args []string) error {
	jsonOut := hasFlag(args, "--json")
	s, err := newScan()
	if err != nil {
		return err
	}
	verdict := capacity.Assess(s.mem, s.inv, s.cfg)
	cands := cleaner.Select(s.cfg, s.snap, s.inv, s.now)

	if jsonOut {
		return emitJSON(map[string]any{
			"memory":             s.mem,
			"capacity":           verdict,
			"worktrees":          s.inv.WithProcs(),
			"cleanup_candidates": len(cands),
			"cleanup_enabled":    s.cfg.CleanupEnabled,
		})
	}

	// ── Shofar art ───────────────────────────────────────────────────────────
	if !hasFlag(args, "--no-art") {
		fmt.Print(shofarArt)
	}

	// ── RAM bar ──────────────────────────────────────────────────────────────
	usedGB := float64(s.mem.UsedBytes) / (1 << 30)
	totalGB := float64(s.mem.TotalBytes) / (1 << 30)
	filled := int(s.mem.UsedBytes * 20 / s.mem.TotalBytes)
	bar := strings.Repeat("▓", filled) + strings.Repeat("░", 20-filled)
	pressureStr := s.mem.PressureName
	if s.mem.Pressure != sysinfo.PressureNormal {
		pressureStr = "⚠ " + pressureStr
	}
	swapStr := ""
	if s.mem.SwapUsedBytes > 0 {
		swapStr = fmt.Sprintf("  swap %s", fmtBytes(s.mem.SwapUsedBytes))
	}
	fmt.Printf("RAM  %.1f / %.1f GB  %s  %s  ·  %s free%s\n",
		usedGB, totalGB, bar, pressureStr, fmtBytes(s.mem.AvailableBytes), swapStr)

	// ── Capacity ──────────────────────────────────────────────────────────────
	capStr := "YES"
	if !verdict.OK {
		capStr = "NO "
	}
	fmt.Printf("     %s — %s\n\n", capStr, verdict.Reason)

	// ── Process groups ────────────────────────────────────────────────────────
	groups := buildGroups(s.snap, s.now)
	if len(groups) > 0 {
		maxName := 6
		for _, g := range groups {
			if n := utf8.RuneCountInString(g.label); n > maxName {
				maxName = n
			}
		}
		if maxName > 28 {
			maxName = 28
		}
		for _, g := range groups {
			hint := ""
			if g.idleCount > 0 {
				hint = fmt.Sprintf("  ← %d idle", g.idleCount)
			}
			fmt.Printf("  %-*s  %3d  %8s%s\n", maxName, g.label, g.count, fmtBytes(g.totalRSS), hint)
			// Sub-line: worktree names for agent sessions
			if len(g.worktrees) > 0 {
				names := g.worktrees
				suffix := ""
				if len(names) > 5 {
					names = names[:5]
					suffix = fmt.Sprintf(", +%d", len(g.worktrees)-5)
				}
				fmt.Printf("    %-*s %s\n", maxName-2, "", strings.Join(names, ", ")+suffix)
			}
		}
		fmt.Println()
	}

	// ── Chrome domains ────────────────────────────────────────────────────────
	domains := chromeDomainsFromAppleScript()
	if len(domains) > 0 {
		fmt.Println("Chrome (domains from open tabs)")
		type domainEntry struct {
			domain string
			tabs   int
		}
		var entries []domainEntry
		tabSum := map[string]int{}
		for _, entry := range domains {
			tabSum[entry.domain] += entry.tabs
		}
		for domain, count := range tabSum {
			entries = append(entries, domainEntry{domain, count})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].tabs > entries[j].tabs })
		if len(entries) > 12 {
			entries = entries[:12]
		}
		for _, e := range entries {
			fmt.Printf("  %-40s %3d tabs\n", e.domain, e.tabs)
		}
		fmt.Println()
	}

	// ── Emdash Worktrees ──────────────────────────────────────────────────────
	emdashWTs := emdashWorktreesFromProcs(s.snap)
	if len(emdashWTs) > 0 {
		fmt.Println("Emdash Worktrees (claude sessions)")
		fmt.Printf("  %-45s %5s %5s\n", "Worktree", "Sess", "RAM")
		fmt.Printf("  %-45s %5s %5s\n", "─────────────────────────────────────────────", "────", "───────")
		for _, e := range emdashWTs {
			wtName := e.name
			if len(wtName) > 45 {
				wtName = wtName[:42] + "..."
			}
			fmt.Printf("  %-45s %5d %5s\n", wtName, e.sessions, fmtBytes(uint64(e.rss)))
		}
		fmt.Println()
	}

	// ── Orphaned/Idle Sessions ─────────────────────────────────────────────────
	orphans := detectOrphanedSessions(s.snap, s.now)
	if len(orphans) > 0 {
		fmt.Println("Orphaned or Idle Sessions (suspicious candidates)")
		fmt.Printf("  %-35s %10s %s\n", "Worktree", "Idle", "Status")
		fmt.Printf("  %-35s %10s %s\n", "─────────────────────────────────", "────────", "─────────────")
		for _, o := range orphans {
			reason := o.reason
			if len(reason) > 20 {
				reason = reason[:17] + "..."
			}
			idleStr := ""
			if o.idleMinutes > 0 {
				idleStr = fmt.Sprintf("%dh %dm", o.idleMinutes/60, o.idleMinutes%60)
			}
			fmt.Printf("  %-35s %10s %s\n", o.wtName, idleStr, reason)
		}
		fmt.Println()
	}

	// ── All Worktrees ────────────────────────────────────────────────────────
	wts := s.inv.WithProcs()
	sort.Slice(wts, func(i, j int) bool { return wts[i].RSSBytes > wts[j].RSSBytes })
	if len(wts) > 0 {
		fmt.Println("All Worktrees")
		fmt.Printf("  %-45s %8s %6s\n", "Name", "RAM", "Procs")
		fmt.Printf("  %-45s %8s %6s\n", "─────────────────────────────────────────────", "───────", "─────")
		for _, wt := range wts {
			activity := ""
			if wt.Active {
				activity = " (active)"
			}
			wtName := wt.Name
			if len(wtName) > 45 {
				wtName = wtName[:42] + "..."
			}
			fmt.Printf("  %-45s %8s %6d%s\n", wtName, fmtBytes(wt.RSSBytes), len(wt.Procs), activity)
		}
		fmt.Println()
	}

	// ── Cleanup ───────────────────────────────────────────────────────────────
	enabled := "off"
	if s.cfg.CleanupEnabled {
		enabled = "on"
	}
	if len(cands) > 0 {
		fmt.Printf("Cleanup    %s  ·  %d candidates  →  shofar clean\n", enabled, len(cands))
	} else {
		fmt.Printf("Cleanup    %s  ·  nothing to kill\n", enabled)
	}
	return nil
}

// processGroup is one row in the process table.
type processGroup struct {
	label     string   // display label, e.g. "claude (Emdash)" or "Chrome"
	count     int
	totalRSS  uint64
	idleCount int
	worktrees []string // distinct worktree names, for agent sessions
}

// skipNames: kernel/system noise not worth surfacing.
var skipNames = map[string]bool{
	"kernel_task": true, "launchd": true, "logd": true, "syslogd": true,
	"UserEventAgent": true, "cfprefsd": true, "distnoted": true,
	"loginwindow": true, "WindowServer": true, "lsregisterurl": true,
	"iconservicesagent": true, "universalaccessd": true,
	"MTLCompilerService": true, "MTLCompilerSe": true,
}

// skipPrefix: process names starting with these are noise.
var skipPrefixes = []string{
	"com.apple.", "com.google.", "com.microsoft.",
	"cloudphotosd", "mediaanalysisd", "remindd",
}

func isNoise(name string) bool {
	if skipNames[name] {
		return true
	}
	lower := strings.ToLower(name)
	for _, p := range skipPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

func buildGroups(snap *proc.Snapshot, now time.Time) []processGroup {
	const minRSS = 50 << 20 // ignore groups under 50 MB
	const topN = 14

	type entry struct {
		totalRSS  uint64
		count     int
		idleCount int
		wtSet     map[string]bool
	}
	byLabel := map[string]*entry{}

	add := func(label string, p *proc.Proc, idle bool, wt string) {
		e := byLabel[label]
		if e == nil {
			e = &entry{wtSet: map[string]bool{}}
			byLabel[label] = e
		}
		e.totalRSS += p.RSSBytes
		e.count++
		if idle {
			e.idleCount++
		}
		if wt != "" {
			e.wtSet[wt] = true
		}
	}

	for _, p := range snap.Procs {
		if p.RSSBytes == 0 {
			continue
		}

		switch p.Kind {
		case proc.KindClaude, proc.KindCodex, proc.KindCursorAgent:
			// Agent CLIs: break down by spawn source so you see Emdash vs tmux vs terminal
			source := spawnSource(p, snap)
			label := string(p.Kind) + " (" + source + ")"

			idle := false
			const idleThresh = 6 * time.Hour
			if p.Kind == proc.KindClaude || p.Kind == proc.KindCodex {
				idleTime, hasTTY := proc.TTYIdle(p, now)
				idle = !hasTTY || (hasTTY && idleTime >= idleThresh)
			}

			wt := cwdShortName(p.Cwd)
			add(label, p, idle, wt)

		default:
			name := displayName(p.Command, p.Kind)
			if name == "" || isNoise(name) {
				continue
			}
			add(name, p, false, "")
		}
	}

	var out []processGroup
	for label, e := range byLabel {
		if e.totalRSS < minRSS {
			continue
		}
		g := processGroup{
			label:    label,
			count:    e.count,
			totalRSS: e.totalRSS,
			idleCount: e.idleCount,
		}
		for wt := range e.wtSet {
			g.worktrees = append(g.worktrees, wt)
		}
		sort.Strings(g.worktrees)
		out = append(out, g)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].totalRSS > out[j].totalRSS })
	if len(out) > topN {
		out = out[:topN]
	}
	return out
}

// spawnSource walks the PPID chain to identify what spawned an agent CLI:
// Emdash, tmux, a known terminal app, or falls back to "terminal".
func spawnSource(p *proc.Proc, snap *proc.Snapshot) string {
	pid := p.PPID
	for depth := 0; depth < 6; depth++ {
		parent, ok := snap.LookupPID(pid)
		if !ok {
			break
		}
		cmd := parent.Command
		name := strings.ToLower(filepath.Base(strings.Fields(cmd)[0]))
		switch {
		case strings.Contains(strings.ToLower(cmd), "emdash"):
			return "Emdash"
		case name == "tmux" || name == "tmux: server":
			return "tmux"
		case name == "terminal", name == "iterm2", name == "alacritty", name == "kitty", name == "warp":
			return name
		}
		pid = parent.PPID
	}
	if p.TTY == "" || p.TTY == "??" {
		return "background"
	}
	return "terminal"
}

// cwdShortName extracts the last path component of a resolved cwd.
// Returns "" when cwd is empty or a boring system path.
func cwdShortName(cwd string) string {
	if cwd == "" || cwd == "/" || strings.HasPrefix(cwd, "/System") || strings.HasPrefix(cwd, "/private") {
		return ""
	}
	return filepath.Base(cwd)
}

// displayName maps a full command line to a human-readable app name.
func displayName(command string, kind proc.Kind) string {
	switch kind {
	case proc.KindDevServer:
		// fall through to name extraction
	case proc.KindTestRunner:
		if strings.Contains(command, "vitest") {
			return "vitest"
		}
		return "jest"
	}

	// macOS app bundles: /Applications/Foo.app/... → "Foo"
	if i := strings.Index(command, ".app/"); i >= 0 {
		prefix := command[:i]
		if j := strings.LastIndex(prefix, "/"); j >= 0 {
			return prefix[j+1:]
		}
		return prefix
	}

	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	base := filepath.Base(fields[0])

	switch {
	case base == "node" && strings.Contains(command, "next"):
		return "next dev"
	case base == "node" && strings.Contains(command, "vite"):
		return "vite"
	case base == "node":
		return "node"
	case base == "npm" || base == "npm-cli.js":
		return "npm"
	case base == "npx":
		return "npx"
	case base == "pnpm" || strings.Contains(command, "pnpm"):
		return "pnpm"
	case strings.HasPrefix(base, "puma"):
		return "puma"
	case base == "ruby" || strings.HasPrefix(base, "ruby3"):
		return "ruby"
	case base == "python3" || base == "python":
		return "python"
	}
	return base
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// chromeDomain is a domain with tab count from Chrome.
type chromeDomain struct {
	domain string
	tabs   int
}

// chromeDomainsFromAppleScript queries Chrome via AppleScript to get open tab domains.
// Returns empty slice if AppleScript fails or Chrome isn't running.
func chromeDomainsFromAppleScript() []chromeDomain {
	script := `tell application "Google Chrome"
  set tabInfo to {}
  repeat with w in windows
    repeat with t in tabs of w
      set end of tabInfo to URL of t
    end repeat
  end repeat
  return tabInfo
end tell`

	cmd := exec.Command("osascript", "-e", script)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	domains := map[string]int{}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "http") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil {
			continue
		}
		host := u.Hostname()
		if host == "" {
			continue
		}
		host = strings.TrimPrefix(host, "www.")
		domains[host]++
	}

	var result []chromeDomain
	for domain, count := range domains {
		result = append(result, chromeDomain{domain, count})
	}
	return result
}

// emdashWT is a worktree with Emdash sessions.
type emdashWT struct {
	name     string
	rss      int64
	sessions int
}

// orphanedSession is a suspicious claude/codex session.
type orphanedSession struct {
	wtName       string
	idleMinutes  int
	reason       string // "reparented", "no TTY", "idle Xh"
}

// emdashWorktreesFromProcs aggregates claude/codex sessions by worktree cwd.
func emdashWorktreesFromProcs(snap *proc.Snapshot) []emdashWT {
	byPath := map[string]*emdashWT{}

	for _, p := range snap.Procs {
		if p.RSSBytes == 0 {
			continue
		}
		if p.Kind != proc.KindClaude && p.Kind != proc.KindCodex {
			continue
		}
		// Only Emdash-spawned sessions
		source := spawnSource(p, snap)
		if source != "Emdash" {
			continue
		}

		// Resolve cwd via lsof if needed
		cwd := p.Cwd
		if cwd == "" {
			binLsof := "/usr/sbin/lsof"
			cmd := exec.Command(binLsof, "-a", "-p", fmt.Sprintf("%d", p.PID), "-d", "cwd", "-Fn")
			out, err := cmd.Output()
			if err == nil {
				for _, line := range strings.Split(string(out), "\n") {
					if strings.HasPrefix(line, "n") {
						cwd = line[1:]
						break
					}
				}
			}
		}

		// Extract worktree name from path: /Users/.../emdash/worktrees/.../<name>
		wtName := ""
		if strings.Contains(cwd, "/worktrees/") {
			if idx := strings.Index(cwd, "/worktrees/"); idx >= 0 {
				rest := cwd[idx+len("/worktrees/"):]
				// Skip repo/team/branch levels, grab the leaf
				parts := strings.Split(rest, "/")
				wtName = parts[len(parts)-1]
			}
		}
		if wtName == "" {
			wtName = filepath.Base(cwd)
		}

		if _, ok := byPath[cwd]; !ok {
			byPath[cwd] = &emdashWT{name: wtName, rss: 0, sessions: 0}
		}
		byPath[cwd].rss += int64(p.RSSBytes)
		byPath[cwd].sessions++
	}

	var result []emdashWT
	for _, wt := range byPath {
		result = append(result, *wt)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].rss > result[j].rss })
	return result
}

// detectOrphanedSessions finds claude/codex sessions that are likely orphaned or stuck.
func detectOrphanedSessions(snap *proc.Snapshot, now time.Time) []orphanedSession {
	const idleThresh = 1 * time.Hour

	var result []orphanedSession

	for _, p := range snap.Procs {
		if p.Kind != proc.KindClaude && p.Kind != proc.KindCodex {
			continue
		}

		wtName := cwdShortName(p.Cwd)
		if wtName == "" {
			wtName = "unknown"
		}

		reason := ""
		idleMin := 0

		// Reparented to launchd (parent died)
		if p.PPID == 1 {
			reason = "reparented (parent died)"
		}

		// No TTY and not actively in use
		if p.TTY == "" || p.TTY == "??" {
			if reason == "" {
				reason = "no TTY"
			} else {
				reason += ", no TTY"
			}
		}

		// Idle via TTY or no recent timestamp
		idleTime, hasTTY := proc.TTYIdle(p, now)
		if hasTTY && idleTime > idleThresh {
			idleMin = int(idleTime.Minutes())
			if reason == "" {
				reason = fmt.Sprintf("idle %dh", idleMin/60)
			} else {
				reason += fmt.Sprintf(", idle %dh", idleMin/60)
			}
		}

		if reason != "" {
			result = append(result, orphanedSession{
				wtName:      wtName,
				idleMinutes: idleMin,
				reason:      reason,
			})
		}
	}

	sort.Slice(result, func(i, j int) bool { return result[i].idleMinutes > result[j].idleMinutes })
	return result
}
