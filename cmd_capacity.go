package main

import (
	"fmt"

	"github.com/astaub/shofar/internal/capacity"
)

func cmdCapacity(args []string) error {
	jsonOut := hasFlag(args, "--json")
	s, err := newScan()
	if err != nil {
		return err
	}
	v := capacity.Assess(s.mem, s.inv, s.cfg)

	if jsonOut {
		return emitJSON(v)
	}

	mark := "NO"
	if v.OK {
		mark = "YES"
	}
	fmt.Printf("%s — %s\n", mark, v.Reason)
	fmt.Printf("  pressure:        %s\n", v.Pressure)
	if v.PressureSticky {
		fmt.Printf("  pressure note:   sticky (swap/compression elevated; usable free memory still healthy)\n")
	}
	fmt.Printf("  available:       %s\n", fmtBytes(v.AvailableBytes))
	fmt.Printf("  reserve:         %s\n", fmtBytes(v.ReserveBytes))
	fmt.Printf("  usable headroom: %s\n", fmtBytes(v.UsableHeadroomBytes))
	fmt.Printf("  worktree budget: %s (%s", fmtBytes(v.PerWorktreeBudgetByte), v.BudgetSource)
	if v.BudgetSource == "measured" {
		fmt.Printf(" from %d worktree(s)", v.MeasuredWorktrees)
	}
	fmt.Printf(")\n  room for:        %d more worktree(s)\n", v.RoomForN)
	return nil
}
