// Package capacity answers the question "can this machine handle another
// worktree + dev server right now?" — the gate no display-only RAM monitor
// provides. The verdict combines three inputs:
//
//   - the kernel's VM pressure level (a hard gate: anything but normal => no)
//   - usable headroom = available memory minus a reserve held for the OS
//   - a per-worktree RAM budget, MEASURED from the footprint of worktrees that
//     currently have processes, falling back to a configured default when there
//     is nothing to measure
//
// "Room for N more" is usable headroom divided by that budget.
package capacity

import (
	"github.com/astaub/shofar/internal/config"
	"github.com/astaub/shofar/internal/sysinfo"
	"github.com/astaub/shofar/internal/worktree"
)

// Verdict is the capacity decision, designed to serialize cleanly for agents.
type Verdict struct {
	OK       bool   `json:"ok"`
	Pressure string `json:"pressure"`
	// PressureSticky distinguishes "genuinely full" from "just conservative".
	// It is true when the kernel reports warning pressure but usable headroom
	// (which already excludes the compressor) still covers at least one
	// worktree — i.e. the pressure is driven by pinned swap / a large
	// compressor backlog rather than exhausted free memory. A caller seeing
	// room_for_n == 0 can read this to tell "truly full" (false) from "the
	// machine is busy but has room" (true).
	PressureSticky        bool   `json:"pressure_sticky"`
	AvailableBytes        uint64 `json:"available_bytes"`
	ReserveBytes          uint64 `json:"reserve_bytes"`
	UsableHeadroomBytes   uint64 `json:"usable_headroom_bytes"`
	PerWorktreeBudgetByte uint64 `json:"per_worktree_budget_bytes"`
	BudgetSource          string `json:"budget_source"` // "measured" | "default"
	MeasuredWorktrees     int    `json:"measured_worktrees"`
	RoomForN              int    `json:"room_for_n"`
	Reason                string `json:"reason"`
}

// Assess computes the capacity verdict from a memory snapshot and the current
// worktree inventory.
func Assess(mem sysinfo.Memory, inv *worktree.Inventory, cfg config.Config) Verdict {
	v := Verdict{
		Pressure:       mem.Pressure.String(),
		AvailableBytes: mem.AvailableBytes,
		ReserveBytes:   cfg.ReserveBytes,
	}

	if mem.AvailableBytes > cfg.ReserveBytes {
		v.UsableHeadroomBytes = mem.AvailableBytes - cfg.ReserveBytes
	}

	budget, source, measured := perWorktreeBudget(inv, cfg)
	v.PerWorktreeBudgetByte = budget
	v.BudgetSource = source
	v.MeasuredWorktrees = measured

	if budget > 0 {
		v.RoomForN = int(v.UsableHeadroomBytes / budget)
	}

	// Critical and unknown pressure fail CLOSED, no matter the arithmetic.
	// Critical means the kernel is aggressively reclaiming and may start killing
	// processes; unknown (e.g. the sysctl could not be read) means we cannot
	// prove it's safe. Approving new work in either case is exactly what shofar
	// exists to prevent.
	switch mem.Pressure {
	case sysinfo.PressureCritical:
		v.OK = false
		v.RoomForN = 0
		v.Reason = "memory pressure is critical; free memory before starting new work"
		return v
	case sysinfo.PressureUnknown:
		v.OK = false
		v.RoomForN = 0
		v.Reason = "memory pressure could not be read; refusing to approve new work while the signal is unknown"
		return v
	}

	// Pressure is now normal or warning, and the usable-headroom arithmetic
	// decides. Warning no longer hard-zeros room_for_n: on a busy dev machine
	// "warning" is often sticky — pinned swap and a large compressor backlog
	// keep the kernel signal elevated while genuinely free memory is still
	// healthy. AvailableBytes already excludes the compressor, and the reserve
	// is held back on top of that, so the arithmetic is the real safety gate
	// here. Hard-gating to 0 under warning left capable machines idle and forced
	// callers to read raw free/swap behind the verdict. Critical still fails
	// closed above.
	v.PressureSticky = mem.Pressure == sysinfo.PressureWarning && v.RoomForN >= 1

	switch {
	case v.PressureSticky:
		v.OK = true
		v.Reason = "memory pressure is warning, but usable headroom covers at least one more worktree; pressure looks driven by swap/compression, not exhausted free memory"
	case v.RoomForN >= 1:
		v.OK = true
		v.Reason = "headroom for at least one more worktree at the current per-worktree budget"
	case mem.Pressure == sysinfo.PressureWarning:
		v.OK = false
		v.Reason = "memory pressure is warning and usable headroom is below one worktree budget; free memory before starting new work"
	default:
		v.OK = false
		v.Reason = "not enough headroom for another worktree at the current per-worktree budget"
	}
	return v
}

// perWorktreeBudget measures the average RSS of worktrees that currently have
// attributed processes. Worktrees below a small floor are ignored so an idle
// worktree with a stray low-memory process doesn't drag the average down. When
// nothing is measurable, the configured default is used.
func perWorktreeBudget(inv *worktree.Inventory, cfg config.Config) (budget uint64, source string, measured int) {
	const floor = 100 << 20 // ignore worktrees using under 100 MiB
	var total uint64
	for _, wt := range inv.WithProcs() {
		if wt.RSSBytes < floor {
			continue
		}
		total += wt.RSSBytes
		measured++
	}
	if measured == 0 {
		return cfg.DefaultWorktreeBudgetBytes, "default", 0
	}
	return total / uint64(measured), "measured", measured
}
