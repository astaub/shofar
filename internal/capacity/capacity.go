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
	OK                    bool   `json:"ok"`
	Pressure              string `json:"pressure"`
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

	// Pressure is a hard gate, and it fails CLOSED. Only an explicit "normal"
	// reading lets the arithmetic decide. Warning/critical means the kernel is
	// already reclaiming; unknown (e.g. the sysctl could not be read) means we
	// can't prove it's safe — both block new work.
	if mem.Pressure != sysinfo.PressureNormal {
		v.OK = false
		v.RoomForN = 0
		if mem.Pressure == sysinfo.PressureUnknown {
			v.Reason = "memory pressure could not be read; refusing to approve new work while the signal is unknown"
		} else {
			v.Reason = "memory pressure is " + mem.Pressure.String() + "; free memory before starting new work"
		}
		return v
	}

	if v.RoomForN >= 1 {
		v.OK = true
		v.Reason = "headroom for at least one more worktree at the current per-worktree budget"
	} else {
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
