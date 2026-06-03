package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/astaub/shofar/internal/capacity"
	"github.com/astaub/shofar/internal/cdp"
	"github.com/astaub/shofar/internal/cleaner"
	"github.com/astaub/shofar/internal/config"
	"github.com/astaub/shofar/internal/proc"
	"github.com/astaub/shofar/internal/sysinfo"
	"github.com/astaub/shofar/internal/worktree"
)

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
	consumers, allProcRSS, agentChildren := buildConsumers(s.snap)
	chrome := chromeBreakdown(s.snap)
	orphans := detectOrphanedSessions(s.snap, s.now)
	recs := buildRecommendations(s, consumers, cands)

	// Per-tab Chrome memory, but only when a debug Chrome is exposing DevTools.
	// Cheap when absent: a closed port refuses the connection instantly. Launch
	// one via `shofar chrome` to populate this.
	var chromeTabs []cdp.Tab
	if cdp.Available(defaultCDPHost, defaultCDPPort) {
		chromeTabs, _ = cdp.Tabs(defaultCDPHost, defaultCDPPort)
		sort.Slice(chromeTabs, func(i, j int) bool { return chromeTabs[i].JSHeapBytes > chromeTabs[j].JSHeapBytes })
	}

	// Agent mode: one structured call exposes everything the human table shows,
	// so an agent can reason or re-render without scraping text.
	if jsonOut {
		return emitJSON(map[string]any{
			"memory":             s.mem,
			"capacity":           verdict,
			"worktrees":          s.inv.WithProcs(),
			"memory_by_ram":      consumers,
			"process_rss_bytes":  allProcRSS,
			"chrome_breakdown":   chrome,
			"agent_breakdown":    agentChildren,
			"chrome_tabs":        chromeTabs,
			"orphans":            orphanJSON(orphans),
			"recommendations":    recs,
			"cleanup_candidates": len(cands),
			"cleanup_enabled":    s.cfg.CleanupEnabled,
		})
	}

	// Decide color BEFORE redirecting stdout to the capture pipe (the pipe is not
	// a terminal). Color is on only for an interactive terminal, off when piped.
	colorOn = !hasFlag(args, "--no-color") &&
		(hasFlag(args, "--color") || os.Getenv("CLICOLOR_FORCE") != "" || isTerminal(os.Stdout))

	// Render the human view into a pipe so we can draw a box around the whole
	// thing at the end. (Output is a few KB — well under the pipe buffer — and a
	// reader goroutine guards against blocking regardless.)
	realStdout := os.Stdout
	pr, pw, perr := os.Pipe()
	if perr != nil {
		return fmt.Errorf("capture output: %w", perr)
	}
	os.Stdout = pw
	rendered := make(chan string, 1)
	go func() { data, _ := io.ReadAll(pr); rendered <- string(data) }()
	// Restore stdout, close the pipe, and draw the box on EVERY exit path —
	// including a panic mid-render — so the reader goroutine never leaks.
	defer func() {
		os.Stdout = realStdout
		pw.Close()
		fmt.Print(boxed(<-rendered))
	}()

	// ── Title ──────────────────────────────────────────────────────────────────
	if !hasFlag(args, "--no-art") {
		fmt.Printf("🐏 %s\n", bold("Shofar"))
		fmt.Println(dim("Take back your RAM"))
		fmt.Println()
	}

	// ── RAM usage chart ───────────────────────────────────────────────────────
	// Lead with the one number that matters: how much of total RAM is in use.
	usedGB := float64(s.mem.UsedBytes) / (1 << 30)
	totalGB := float64(s.mem.TotalBytes) / (1 << 30)
	usedPctTotal := int(s.mem.UsedBytes * 100 / s.mem.TotalBytes)
	const barW = 28
	filled := int(s.mem.UsedBytes * barW / s.mem.TotalBytes)
	if filled > barW {
		filled = barW
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barW-filled)
	pcolor := pressureColor(s.mem.Pressure)
	pressureStr := s.mem.PressureName
	if s.mem.Pressure != sysinfo.PressureNormal {
		pressureStr = "⚠ " + pressureStr
	}
	fmt.Printf("RAM  %s used  ·  %.1f of %.1f GB\n", bold(fmt.Sprintf("%d%%", usedPctTotal)), usedGB, totalGB)
	fmt.Printf("  [%s]\n", pcolor(bar))
	fmt.Printf("  %s free  ·  swap %s  ·  pressure %s\n\n",
		fmtBytes(s.mem.AvailableBytes), fmtBytes(s.mem.SwapUsedBytes), pcolor(pressureStr))

	// ── Capacity ──────────────────────────────────────────────────────────────
	capStr := green("YES")
	if !verdict.OK {
		capStr = red("NO")
	}
	fmt.Printf("Capacity  %s — %s\n\n", capStr, verdict.Reason)

	// ── Used RAM breakdown ────────────────────────────────────────────────────
	// These three categories are the kernel's own accounting (wired + active +
	// compressed = UsedBytes), so unlike the per-app RSS table below they sum to
	// exactly 100% of used — they answer "where did the RAM actually go" without
	// the cross-app double-counting that RSS suffers from.
	usedPct := func(b uint64) float64 {
		if s.mem.UsedBytes == 0 {
			return 0
		}
		return float64(b) * 100 / float64(s.mem.UsedBytes)
	}
	type memCat struct {
		name, note string
		bytes      uint64
	}
	cats := []memCat{
		{"Compressed", "cold app pages held in RAM", s.mem.CompressedBytes},
		{"Active", "live app working set", s.mem.ActiveBytes},
		{"Wired", "kernel, drivers, GPU — not reclaimable", s.mem.WiredBytes},
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i].bytes > cats[j].bytes })
	fmt.Printf("Used RAM  %s of %s  (%.0f%% of physical)\n",
		fmtBytes(s.mem.UsedBytes), fmtBytes(s.mem.TotalBytes),
		float64(s.mem.UsedBytes)*100/float64(s.mem.TotalBytes))
	for _, c := range cats {
		note := c.note
		if c.name == "Compressed" && c.bytes > 0 {
			note = fmt.Sprintf("%s (~2:1, ≈%s uncompressed)", c.note, fmtBytes(c.bytes*2))
		}
		fmt.Printf("  %-11s %9s  %4.0f%%   %s\n", c.name, fmtBytes(c.bytes), usedPct(c.bytes), note)
	}
	if s.mem.SwapUsedBytes > 0 {
		fmt.Printf("  %-11s %9s   ——    backed by disk; shrinks only as demand drops\n", "+ Swap", fmtBytes(s.mem.SwapUsedBytes))
	}
	fmt.Println()

	// ── What's using your RAM ─────────────────────────────────────────────────
	// One ranked leaderboard of what's actually consuming memory, biggest first.
	// Each row is the unit you'd act on: agent sessions are grouped by their
	// WORKTREE, everything else by app. Percentages are a share of total process
	// RSS — a separate lens from the Used breakdown above (RSS double-counts
	// shared pages and ignores the compressor, so it does not map onto UsedBytes).
	if len(consumers) > 0 {
		const nameW = 40
		const maxRows = 32
		floor := allProcRSS / 100 // show apps/worktrees using at least 1% of process RAM
		pctOf := func(b uint64) float64 {
			if allProcRSS == 0 {
				return 0
			}
			return float64(b) * 100 / float64(allProcRSS)
		}
		row := func(name, kind, ram, pctStr string) {
			fmt.Printf("  %s  %s %10s  %s\n", padRight(name, nameW), colorKind(padRight(kind, 8)), ram, pctStr)
		}
		fmt.Println("What's using your RAM  (biggest first)")
		row("Name", "kind", "RAM", "%")
		row(strings.Repeat("─", nameW), "────────", "──────────", "─────")
		var shownRSS uint64
		shownGroups := 0
		for _, c := range consumers {
			if c.RSSBytes < floor || shownGroups >= maxRows {
				break
			}
			shownGroups++
			shownRSS += c.RSSBytes
			label := c.Label
			kids, isAgent := agentChildren[c.Label]
			switch {
			case isAgent:
				label = fmt.Sprintf("%s  ·  %d worktrees", c.Label, len(kids))
			case c.Procs > 1:
				label = fmt.Sprintf("%s (x%d)", label, c.Procs)
			}
			row(truncRunes(label, nameW), c.Kind, fmtBytes(c.RSSBytes), fmt.Sprintf("%.1f%%", pctOf(c.RSSBytes)))

			// Drill-downs: Chrome by process type, agents by worktree.
			switch {
			case c.Label == "Google Chrome":
				for i, part := range chrome {
					conn := "├ "
					if i == len(chrome)-1 {
						conn = "└ "
					}
					row("    "+conn+part.Label, "", fmtBytes(part.RSSBytes), "")
				}
			case isAgent:
				more := 0
				if len(kids) > 12 {
					more = len(kids) - 12
					kids = kids[:12]
				}
				for i, k := range kids {
					conn := "├ "
					if i == len(kids)-1 && more == 0 {
						conn = "└ "
					}
					kl := k.Label
					if k.Procs > 1 {
						kl = fmt.Sprintf("%s (x%d)", kl, k.Procs)
					}
					row("    "+conn+truncRunes(kl, nameW-6), "", fmtBytes(k.RSSBytes), "")
				}
				if more > 0 {
					row(fmt.Sprintf("    └ +%d more worktrees", more), "", "", "")
				}
			}
		}
		if other := allProcRSS - shownRSS; other > 0 {
			label := "other (smaller procs)"
			if hidden := len(consumers) - shownGroups; hidden > 0 {
				label = fmt.Sprintf("other (%d more, each <1%%)", hidden)
			}
			row(label, "", fmtBytes(other), fmt.Sprintf("%.1f%%", pctOf(other)))
		}
		row("all processes", "", fmtBytes(allProcRSS), "100%")
		fmt.Println()
	}

	// ── Chrome tabs by memory (only when a debug Chrome is attached) ───────────
	if len(chromeTabs) > 0 {
		const nameW = 44
		fmt.Printf("Chrome tabs by memory  (JS heap · debug Chrome on :%d)\n", defaultCDPPort)
		fmt.Printf("  %s  %10s\n", padRight("Tab", nameW), "JS HEAP")
		fmt.Printf("  %s  %10s\n", strings.Repeat("─", nameW), "──────────")
		var total uint64
		for _, t := range chromeTabs {
			total += t.JSHeapBytes
			label := t.Title
			if label == "" {
				label = t.URL
			}
			if h := hostOf(t.URL); h != "" {
				label = truncRunes(label, nameW-len(h)-3) + " — " + h
			}
			fmt.Printf("  %s  %10s\n", padRight(truncRunes(label, nameW), nameW), fmtBytes(t.JSHeapBytes))
		}
		fmt.Printf("  %s  %10s\n", padRight(fmt.Sprintf("%d tabs", len(chromeTabs)), nameW), fmtBytes(total))
		fmt.Println()
	}

	// ── Orphaned/Idle Sessions ─────────────────────────────────────────────────
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

	// ── To reclaim RAM (recommendations) ──────────────────────────────────────
	// shofar's job is to turn the numbers above into actions. These are generated
	// from the live scan: what's heavy, what's safe to kill, and what to do about
	// the apps it can't kill for you (browsers).
	if len(recs) > 0 {
		head := bold("Reclaim RAM")
		if s.mem.Pressure == sysinfo.PressureNormal {
			head += dim("  (optional — headroom is fine)")
		}
		fmt.Println(head)
		w := 0
		for _, r := range recs {
			if n := visibleWidth(r.Target); n > w {
				w = n
			}
		}
		for _, r := range recs {
			fmt.Printf("  %s  →  %s\n", bold(padRight(r.Target, w)), r.Action)
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
	return nil // the deferred restore draws the boxed output
}

// boxed wraps multi-line text in a rounded box sized to its widest line,
// measuring *visible* width so ANSI color codes don't throw off the borders.
func boxed(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	w := 0
	for _, ln := range lines {
		if n := visibleWidth(ln); n > w {
			w = n
		}
	}
	var b strings.Builder
	b.WriteString("╭" + strings.Repeat("─", w+2) + "╮\n")
	for _, ln := range lines {
		b.WriteString("│ " + ln + strings.Repeat(" ", w-visibleWidth(ln)) + " │\n")
	}
	b.WriteString("╰" + strings.Repeat("─", w+2) + "╯\n")
	return b.String()
}

// visibleWidth counts displayed columns, skipping ANSI SGR escape sequences
// (ESC [ … m) so colored text still aligns in fixed-width tables and the box.
func visibleWidth(s string) int {
	w, inEsc := 0, false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == 0x1b:
			inEsc = true
		default:
			w++
		}
	}
	return w
}

// ── color (gated on an interactive terminal via colorOn) ────────────────────

var colorOn bool

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func sgr(code, s string) string {
	if !colorOn {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func bold(s string) string   { return sgr("1", s) }
func dim(s string) string    { return sgr("2", s) }
func red(s string) string    { return sgr("31", s) }
func green(s string) string  { return sgr("32", s) }
func yellow(s string) string { return sgr("33", s) }
func cyan(s string) string   { return sgr("36", s) }

// pressureColor returns the color matching a memory-pressure level.
func pressureColor(p sysinfo.Pressure) func(string) string {
	switch p {
	case sysinfo.PressureWarning:
		return yellow
	case sysinfo.PressureCritical:
		return red
	case sysinfo.PressureNormal:
		return green
	default:
		return func(s string) string { return s }
	}
}

// colorKind tints the "kind" cell: agent sessions (claude/codex/cursor) cyan so
// they stand out from plain apps.
func colorKind(k string) string {
	switch strings.TrimSpace(k) {
	case "claude", "codex", "cursor":
		return cyan(k)
	default:
		return k
	}
}

// padRight pads s with spaces to n display columns, counting runes (not bytes)
// so multibyte names like "@architect" or "x4" stay aligned in the table.
func padRight(s string, n int) string {
	if w := utf8.RuneCountInString(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// truncRunes shortens s to at most n runes, appending an ellipsis when cut, so
// fixed-width table columns don't blow out on long app names.
func truncRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// consumer is one row of the unified "what's using your RAM" leaderboard. Agent
// sessions are grouped by the WORKTREE they run in (the unit you act on); every
// other process is grouped by app. This deliberately mixes units — the question
// is "where is the RAM going", and the most meaningful unit differs per consumer.
type consumer struct {
	Label    string `json:"label"`
	Kind     string `json:"kind"` // "app", "worktree", or "agent"
	Procs    int    `json:"procs"`
	RSSBytes uint64 `json:"rss_bytes"`
}

// buildConsumers groups every process into a single ranked list, returns the
// total process RSS (the denominator), and a per-agent-kind breakdown by
// worktree. Apps group by name; agent CLIs (claude/codex/cursor) collapse into
// ONE parent row per kind, with each worktree tracked as a child shown indented
// — mirroring the Chrome drill-down, so the agents don't scatter through the list.
func buildConsumers(snap *proc.Snapshot) (rows []consumer, allProcRSS uint64, agentChildren map[string][]consumer) {
	type agg struct {
		kind  string
		count int
		rss   uint64
	}
	byKey := map[string]*agg{}               // app name OR agent kind → totals
	childAgg := map[string]map[string]*agg{} // agent kind → worktree → totals
	for _, p := range snap.Procs {
		allProcRSS += p.RSSBytes
		if p.RSSBytes == 0 {
			continue
		}
		switch p.Kind {
		case proc.KindClaude, proc.KindCodex, proc.KindCursorAgent:
			ak := agentKind(p.Kind)
			a := byKey[ak]
			if a == nil {
				a = &agg{kind: ak}
				byKey[ak] = a
			}
			a.count++
			a.rss += p.RSSBytes
			wt := worktreeLabel(p.Cwd)
			if wt == "" {
				wt = "(no worktree)"
			}
			if childAgg[ak] == nil {
				childAgg[ak] = map[string]*agg{}
			}
			c := childAgg[ak][wt]
			if c == nil {
				c = &agg{kind: ak}
				childAgg[ak][wt] = c
			}
			c.count++
			c.rss += p.RSSBytes
		default:
			name := displayName(p.Command, p.Kind)
			if name == "" || isNoise(name) {
				continue
			}
			a := byKey[name]
			if a == nil {
				a = &agg{kind: "app"}
				byKey[name] = a
			}
			a.count++
			a.rss += p.RSSBytes
		}
	}
	for label, a := range byKey {
		rows = append(rows, consumer{Label: label, Kind: a.kind, Procs: a.count, RSSBytes: a.rss})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RSSBytes > rows[j].RSSBytes })

	agentChildren = map[string][]consumer{}
	for ak, m := range childAgg {
		var ch []consumer
		for wt, a := range m {
			ch = append(ch, consumer{Label: wt, Kind: a.kind, Procs: a.count, RSSBytes: a.rss})
		}
		sort.Slice(ch, func(i, j int) bool { return ch[i].RSSBytes > ch[j].RSSBytes })
		agentChildren[ak] = ch
	}
	return rows, allProcRSS, agentChildren
}

// chromePart is one sub-row of the Chrome breakdown (tabs, extensions, etc.).
type chromePart struct {
	Label    string `json:"label"`
	RSSBytes uint64 `json:"rss_bytes"`
}

// chromeBreakdown splits Chrome's many processes into meaningful buckets using
// the --type / --extension-process flags Chrome puts on its helper command
// lines. The buckets sum to the "Google Chrome" leaderboard row. Tabs and
// extensions are the actionable ones: tabs → chrome://discards, extensions →
// chrome://extensions.
func chromeBreakdown(snap *proc.Snapshot) []chromePart {
	var tabs, exts, gpu, util, core uint64
	var tabN, extN int
	for _, p := range snap.Procs {
		if displayName(p.Command, p.Kind) != "Google Chrome" {
			continue
		}
		cmd := p.Command
		switch {
		case strings.Contains(cmd, "--extension-process"):
			exts += p.RSSBytes
			extN++
		case strings.Contains(cmd, "--type=renderer"):
			tabs += p.RSSBytes
			tabN++
		case strings.Contains(cmd, "--type=gpu-process"):
			gpu += p.RSSBytes
		case strings.Contains(cmd, "--type="):
			util += p.RSSBytes // network/storage/audio/zygote utilities
		default:
			core += p.RSSBytes // the main browser process
		}
	}
	var parts []chromePart
	add := func(l string, b uint64) {
		if b > 0 {
			parts = append(parts, chromePart{Label: l, RSSBytes: b})
		}
	}
	add(fmt.Sprintf("tab + iframe renderers (x%d)", tabN), tabs)
	add(fmt.Sprintf("extensions (x%d)", extN), exts)
	add("GPU", gpu)
	add("utilities", util)
	add("browser core", core)
	sort.Slice(parts, func(i, j int) bool { return parts[i].RSSBytes > parts[j].RSSBytes })
	return parts
}

// orphanJSON converts orphan rows into a serializable form for agent mode.
func orphanJSON(orphans []orphanedSession) []map[string]any {
	out := make([]map[string]any, 0, len(orphans))
	for _, o := range orphans {
		out = append(out, map[string]any{
			"worktree":     o.wtName,
			"idle_minutes": o.idleMinutes,
			"status":       o.reason,
		})
	}
	return out
}

// worktreeLabel derives a worktree name from an agent process's cwd, preferring
// the leaf under a ".../worktrees/<...>/<name>" layout (Emdash nests two levels
// deep, plain git worktrees one). Returns "" when the cwd is unknown.
func worktreeLabel(cwd string) string {
	if cwd == "" {
		return ""
	}
	if i := strings.LastIndex(cwd, "/worktrees/"); i >= 0 {
		rest := cwd[i+len("/worktrees/"):]
		parts := strings.Split(rest, "/")
		return parts[len(parts)-1]
	}
	return filepath.Base(cwd)
}

// agentKind maps an agent process kind to a short label for the "kind" column.
func agentKind(k proc.Kind) string {
	if k == proc.KindCursorAgent {
		return "cursor"
	}
	return string(k) // "claude" or "codex"
}

// buildRecommendations turns the live scan into concrete, ranked actions. The
// emphasis is on what shofar can't do for you: browsers can't be safely killed,
// so it tells you how to shrink them in place; stale processes it CAN kill it
// points at `shofar clean`.
func buildRecommendations(s *scan, consumers []consumer, cands []cleaner.Candidate) []recItem {
	var recs []recItem

	// Browser: heavy, can't be auto-killed — point at the in-place shrink.
	for _, c := range consumers {
		if c.Label == "Google Chrome" && c.RSSBytes > 1500<<20 {
			recs = append(recs, recItem{
				Target: "Chrome " + fmtBytes(c.RSSBytes),
				Action: "Memory Saver + trim extensions (chrome://settings/performance)",
			})
			break
		}
	}

	// Safe-to-kill stale processes, with their true subtree reclaim.
	if len(cands) > 0 {
		var rb uint64
		for _, c := range cands {
			rb += c.ReclaimBytes
		}
		recs = append(recs, recItem{
			Target: "Stale procs ~" + fmtBytes(rb),
			Action: fmt.Sprintf("shofar clean --kill  (%d safe)", len(cands)),
		})
	}

	// Heavy swap: explain it won't drain on its own.
	if s.mem.SwapUsedBytes > 8<<30 {
		recs = append(recs, recItem{
			Target: "Swap " + fmtBytes(s.mem.SwapUsedBytes),
			Action: "only drains as you free real RAM",
		})
	}

	return recs
}

// recItem is one concise reclaim suggestion — a target (with size) and the
// action. Terse and column-aligned so the list scans at a glance.
type recItem struct {
	Target string `json:"target"`
	Action string `json:"action"`
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

// orphanedSession is a suspicious claude/codex session.
type orphanedSession struct {
	wtName      string
	idleMinutes int
	reason      string // "reparented", "no TTY", "idle Xh"
}

// detectOrphanedSessions finds claude/codex sessions that are genuinely orphaned
// or idle. It judges a session by its whole process subtree, mirroring the
// cleaner: a detached session (a `tmux new-session -d ... claude` launcher with
// a live child) is NOT suspicious just because the launcher lost its TTY. Only
// sessions where nothing in the tree has touched a terminal recently are
// surfaced — so this list stays consistent with what `shofar clean` would act
// on, instead of crying wolf on every no-TTY Emdash session.
func detectOrphanedSessions(snap *proc.Snapshot, now time.Time) []orphanedSession {
	const idleThresh = 1 * time.Hour

	var result []orphanedSession

	for _, p := range snap.Procs {
		if p.Kind != proc.KindClaude && p.Kind != proc.KindCodex {
			continue
		}
		// Session roots only — skip the inner process of a detached launcher.
		if parent, ok := snap.LookupPID(p.PPID); ok &&
			(parent.Kind == proc.KindClaude || parent.Kind == proc.KindCodex) {
			continue
		}

		wtName := cwdShortName(p.Cwd)
		if wtName == "" {
			wtName = "unknown"
		}

		idle, hasTTY := snap.SubtreeMinTTYIdle(p.PID, now)
		reason := ""
		idleMin := 0
		switch {
		case !hasTTY && p.PPID == 1:
			// Nothing in the tree has a terminal and the parent died: true orphan.
			reason = "orphaned (no TTY, parent died)"
		case hasTTY && idle >= idleThresh:
			idleMin = int(idle.Minutes())
			reason = fmt.Sprintf("idle %dh %dm", idleMin/60, idleMin%60)
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
