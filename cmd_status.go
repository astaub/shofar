package main

import (
	"encoding/json"
	"fmt"
	"os"
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

// scan bundles the work shared by status/capacity/clean: read config, memory,
// processes, and the worktree inventory at one moment.
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

const shofarArt = `
     ,_.
    (    '-.
   /         '-.._
  /                '--._
 /                      '-._
|                           '-.
 \                             )
  '-.._                      /
        '--.._          _.--'
               '--------'
                   |
                  (___)

`

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

	m := s.mem

	// ── Shofar art ──────────────────────────────────────────────────────────
	if !hasFlag(args, "--no-art") {
		fmt.Print(shofarArt)
	}

	// ── RAM header ──────────────────────────────────────────────────────────
	usedGB := float64(m.UsedBytes) / (1 << 30)
	totalGB := float64(m.TotalBytes) / (1 << 30)
	pct := m.UsedBytes * 20 / m.TotalBytes // 20-char bar
	bar := strings.Repeat("▓", int(pct)) + strings.Repeat("░", 20-int(pct))
	pressureStr := m.PressureName
	if m.Pressure != sysinfo.PressureNormal {
		pressureStr = "⚠ " + pressureStr
	}
	swapStr := ""
	if m.SwapUsedBytes > 0 {
		swapStr = fmt.Sprintf("  swap %s", fmtBytes(m.SwapUsedBytes))
	}
	fmt.Printf("RAM  %.1f / %.1f GB  %s  %s  ·  %s free%s\n",
		usedGB, totalGB, bar, pressureStr, fmtBytes(m.AvailableBytes), swapStr)

	// ── Capacity ─────────────────────────────────────────────────────────────
	capStr := "YES"
	if !verdict.OK {
		capStr = "NO "
	}
	fmt.Printf("     %s — %s\n\n", capStr, verdict.Reason)

	// ── Process groups ───────────────────────────────────────────────────────
	groups := buildGroups(s.snap.Procs, s.now)
	if len(groups) > 0 {
		// Column widths
		maxName := 0
		for _, g := range groups {
			if n := utf8.RuneCountInString(g.name); n > maxName {
				maxName = n
			}
		}
		if maxName < 6 {
			maxName = 6
		}
		maxName = min(maxName, 24)

		for _, g := range groups {
			countStr := fmt.Sprintf("%d", g.count)
			rssStr := fmtBytes(g.totalRSS)
			hint := ""
			if g.idleCount > 0 {
				hint = fmt.Sprintf("  ← %d idle · shofar clean to reclaim ~%s", g.idleCount, fmtBytes(g.idleRSS))
			}
			fmt.Printf("  %-*s  %3s  %8s%s\n", maxName, g.name, countStr, rssStr, hint)
		}
		fmt.Println()
	}

	// ── Worktrees ─────────────────────────────────────────────────────────────
	wts := s.inv.WithProcs()
	sort.Slice(wts, func(i, j int) bool { return wts[i].RSSBytes > wts[j].RSSBytes })
	if len(wts) > 0 {
		fmt.Println("Worktrees")
		for _, wt := range wts {
			activity := ""
			if wt.Active {
				activity = "  active"
			}
			fmt.Printf("  %-28s  %8s  %d procs%s\n", wt.Name, fmtBytes(wt.RSSBytes), len(wt.Procs), activity)
		}
	} else {
		hint := ""
		if len(s.cfg.WorktreeBases) == 0 || (len(s.cfg.WorktreeBases) == 1 && s.cfg.WorktreeBases[0] == defaultWorktreeBase()) {
			hint = "  ·  add worktree_bases to ~/.config/shofar/config.json for detail"
		}
		fmt.Printf("Worktrees  none%s\n", hint)
	}

	// ── Cleanup ───────────────────────────────────────────────────────────────
	enabled := "off"
	if s.cfg.CleanupEnabled {
		enabled = "on"
	}
	if len(cands) > 0 {
		fmt.Printf("Cleanup    %s  ·  %d candidates  →  run shofar clean\n", enabled, len(cands))
	} else {
		fmt.Printf("Cleanup    %s  ·  nothing to kill\n", enabled)
	}
	return nil
}

// processGroup is a named aggregate of processes (e.g. "Chrome", "claude").
type processGroup struct {
	name      string
	count     int
	totalRSS  uint64
	idleCount int    // agent CLIs idle past threshold
	idleRSS   uint64 // RSS attributable to idle sessions
}

// skipNames are kernel/system processes not worth showing.
var skipNames = map[string]bool{
	"kernel_task": true, "launchd": true, "logd": true,
	"UserEventAgent": true, "cfprefsd": true, "distnoted": true,
	"lsregisterurl": true, "coredata": true, "iconservices": true,
	"loginwindow": true, "WindowServer": true,
}

// buildGroups aggregates processes by display name, sorted by total RSS.
// Only the top 12 groups are returned to keep output readable.
func buildGroups(procs []*proc.Proc, now time.Time) []processGroup {
	const minRSS = 50 << 20  // ignore groups under 50 MB
	const topN = 12

	byName := map[string]*processGroup{}
	for _, p := range procs {
		if p.RSSBytes == 0 {
			continue
		}
		name := displayName(p.Command, p.Kind)
		if skipNames[name] || name == "" {
			continue
		}
		g := byName[name]
		if g == nil {
			g = &processGroup{name: name}
			byName[name] = g
		}
		g.count++
		g.totalRSS += p.RSSBytes

		// Mark idle agent CLIs for the cleanup hint
		if p.Kind == proc.KindClaude || p.Kind == proc.KindCodex {
			idle, hasTTY := proc.TTYIdle(p, now)
			const idleThreshold = 6 * time.Hour
			if !hasTTY || (hasTTY && idle >= idleThreshold) {
				g.idleCount++
				g.idleRSS += p.RSSBytes
			}
		}
	}

	var out []processGroup
	for _, g := range byName {
		if g.totalRSS >= minRSS {
			out = append(out, *g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].totalRSS > out[j].totalRSS })
	if len(out) > topN {
		out = out[:topN]
	}
	return out
}

// displayName extracts a human-readable name from a process command line.
// macOS app bundles like /Applications/Foo.app/Contents/MacOS/Foo → "Foo".
// Homebrew/system binaries use just the base name.
func displayName(command string, kind proc.Kind) string {
	switch kind {
	case proc.KindClaude:
		return "claude"
	case proc.KindCodex:
		return "codex"
	case proc.KindCursorAgent:
		return "cursor-agent"
	case proc.KindDevServer:
		// Extract a readable name for dev servers
	case proc.KindTestRunner:
		if strings.Contains(command, "vitest") {
			return "vitest"
		}
		return "jest"
	}

	// Strip /Applications/Foo.app/... → "Foo"
	if i := strings.Index(command, ".app/"); i >= 0 {
		prefix := command[:i]
		if j := strings.LastIndex(prefix, "/"); j >= 0 {
			return prefix[j+1:]
		}
		return prefix
	}

	// Use first word, base name
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	base := filepath.Base(fields[0])

	// Consolidate common patterns
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
	case base == "ruby" || base == "ruby3.2" || base == "ruby3.3":
		return "ruby"
	case base == "python3" || base == "python":
		return "python"
	}
	return base
}

func defaultWorktreeBase() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "code", "worktrees")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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
