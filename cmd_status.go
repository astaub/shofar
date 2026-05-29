package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

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
	fmt.Printf("Memory   %s used / %s total  (%s available, pressure: %s",
		fmtBytes(m.UsedBytes), fmtBytes(m.TotalBytes), fmtBytes(m.AvailableBytes), m.PressureName)
	if m.SwapUsedBytes > 0 {
		fmt.Printf(", swap %s", fmtBytes(m.SwapUsedBytes))
	}
	fmt.Println(")")

	mark := "no"
	if verdict.OK {
		mark = "yes"
	}
	fmt.Printf("Capacity %s — room for %d more worktree(s) (%s/worktree, %s budget)\n",
		mark, verdict.RoomForN, fmtBytes(verdict.PerWorktreeBudgetByte), verdict.BudgetSource)

	wts := s.inv.WithProcs()
	sort.Slice(wts, func(i, j int) bool { return wts[i].RSSBytes > wts[j].RSSBytes })
	if len(wts) > 0 {
		fmt.Println("\nWorktrees with processes:")
		for _, wt := range wts {
			tag := ""
			if wt.Active {
				tag = "  [active: " + wt.ActiveReason + "]"
			}
			fmt.Printf("  %-28s %8s  %d proc(s)%s\n", wt.Name, fmtBytes(wt.RSSBytes), len(wt.Procs), tag)
		}
	}

	enabled := "off"
	if s.cfg.CleanupEnabled {
		enabled = "on"
	}
	fmt.Printf("\nCleanup  %s — %d safe-to-kill process(es) right now", enabled, len(cands))
	if len(cands) > 0 {
		fmt.Print("  (run `shofar clean` to review)")
	}
	fmt.Println()
	return nil
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
