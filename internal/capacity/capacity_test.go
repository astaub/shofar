package capacity

import (
	"testing"

	"github.com/astaub/shofar/internal/config"
	"github.com/astaub/shofar/internal/proc"
	"github.com/astaub/shofar/internal/sysinfo"
	"github.com/astaub/shofar/internal/worktree"
)

func baseCfg() config.Config {
	c := config.Default()
	c.ReserveBytes = 3 << 30
	c.DefaultWorktreeBudgetBytes = 1 << 30
	return c
}

func TestAssess_PressureHardGate(t *testing.T) {
	mem := sysinfo.Memory{
		TotalBytes:     24 << 30,
		AvailableBytes: 20 << 30, // plenty of room arithmetically
		Pressure:       sysinfo.PressureWarning,
	}
	v := Assess(mem, &worktree.Inventory{}, baseCfg())
	if v.OK {
		t.Error("expected OK=false under warning pressure despite headroom")
	}
	if v.RoomForN != 0 {
		t.Errorf("RoomForN = %d, want 0 under pressure", v.RoomForN)
	}
}

func TestAssess_UnknownPressureFailsClosed(t *testing.T) {
	// A failed pressure read (PressureUnknown) must block, not approve.
	mem := sysinfo.Memory{
		TotalBytes:     24 << 30,
		AvailableBytes: 20 << 30, // plenty of headroom arithmetically
		Pressure:       sysinfo.PressureUnknown,
	}
	v := Assess(mem, &worktree.Inventory{}, baseCfg())
	if v.OK {
		t.Error("expected OK=false when pressure is unknown (fail closed)")
	}
	if v.RoomForN != 0 {
		t.Errorf("RoomForN = %d, want 0 when pressure unknown", v.RoomForN)
	}
}

func TestAssess_DefaultBudget(t *testing.T) {
	mem := sysinfo.Memory{
		TotalBytes:     24 << 30,
		AvailableBytes: 8 << 30, // 8 - 3 reserve = 5 usable, /1GB = 5
		Pressure:       sysinfo.PressureNormal,
	}
	v := Assess(mem, &worktree.Inventory{}, baseCfg())
	if !v.OK {
		t.Error("expected OK=true")
	}
	if v.BudgetSource != "default" {
		t.Errorf("BudgetSource = %q, want default", v.BudgetSource)
	}
	if v.RoomForN != 5 {
		t.Errorf("RoomForN = %d, want 5", v.RoomForN)
	}
}

func TestAssess_MeasuredBudget(t *testing.T) {
	inv := &worktree.Inventory{Worktrees: []*worktree.Worktree{
		{Name: "a", RSSBytes: 2 << 30, Procs: dummyProcs()},
		{Name: "b", RSSBytes: 4 << 30, Procs: dummyProcs()},
		{Name: "idle", RSSBytes: 10 << 20, Procs: dummyProcs()}, // below floor, ignored
	}}
	mem := sysinfo.Memory{
		TotalBytes:     32 << 30,
		AvailableBytes: 12 << 30, // 12 - 3 = 9 usable
		Pressure:       sysinfo.PressureNormal,
	}
	v := Assess(mem, inv, baseCfg())
	if v.BudgetSource != "measured" {
		t.Fatalf("BudgetSource = %q, want measured", v.BudgetSource)
	}
	if v.MeasuredWorktrees != 2 {
		t.Errorf("MeasuredWorktrees = %d, want 2 (idle below floor)", v.MeasuredWorktrees)
	}
	// avg of 2GB and 4GB = 3GB; 9 usable / 3 = 3
	if v.RoomForN != 3 {
		t.Errorf("RoomForN = %d, want 3", v.RoomForN)
	}
}

func TestAssess_NoHeadroom(t *testing.T) {
	mem := sysinfo.Memory{
		TotalBytes:     8 << 30,
		AvailableBytes: 2 << 30, // below the 3GB reserve
		Pressure:       sysinfo.PressureNormal,
	}
	v := Assess(mem, &worktree.Inventory{}, baseCfg())
	if v.OK {
		t.Error("expected OK=false with no usable headroom")
	}
	if v.UsableHeadroomBytes != 0 {
		t.Errorf("UsableHeadroomBytes = %d, want 0", v.UsableHeadroomBytes)
	}
}

// dummyProcs gives a worktree a non-empty Procs slice so WithProcs counts it.
func dummyProcs() []*proc.Proc { return []*proc.Proc{{PID: 1}} }
