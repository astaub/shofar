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

func TestAssess_WarningStickyAllowsHealthyHeadroom(t *testing.T) {
	// The field case from the Staub orchestrator: warning pressure (pinned swap
	// + large compressor backlog) while genuinely free memory is still healthy.
	// Warning must NOT hard-zero room_for_n here — the arithmetic decides and
	// the verdict is flagged sticky so a caller knows 0-would-be-conservative.
	mem := sysinfo.Memory{
		TotalBytes:     24 << 30,
		AvailableBytes: 8 << 30, // 8 - 3 reserve = 5 usable, /1GB = 5
		Pressure:       sysinfo.PressureWarning,
	}
	v := Assess(mem, &worktree.Inventory{}, baseCfg())
	if !v.OK {
		t.Error("expected OK=true under warning when usable headroom is healthy")
	}
	if v.RoomForN != 5 {
		t.Errorf("RoomForN = %d, want 5 (warning must not hard-zero healthy headroom)", v.RoomForN)
	}
	if !v.PressureSticky {
		t.Error("expected PressureSticky=true (warning + healthy headroom)")
	}
}

func TestAssess_WarningTightStillBlocks(t *testing.T) {
	// Warning pressure AND usable headroom below one budget: the kernel signal
	// and the arithmetic agree the machine is full. Block, and do NOT flag
	// sticky — this 0 is genuine.
	mem := sysinfo.Memory{
		TotalBytes:     8 << 30,
		AvailableBytes: 2 << 30, // below the 3GB reserve => 0 usable
		Pressure:       sysinfo.PressureWarning,
	}
	v := Assess(mem, &worktree.Inventory{}, baseCfg())
	if v.OK {
		t.Error("expected OK=false under warning with no usable headroom")
	}
	if v.RoomForN != 0 {
		t.Errorf("RoomForN = %d, want 0", v.RoomForN)
	}
	if v.PressureSticky {
		t.Error("expected PressureSticky=false when headroom is genuinely tight")
	}
}

func TestAssess_CriticalHardGate(t *testing.T) {
	// Critical fails closed regardless of arithmetic headroom, and is never
	// flagged sticky — critical is never "just conservative".
	mem := sysinfo.Memory{
		TotalBytes:     24 << 30,
		AvailableBytes: 20 << 30, // plenty of room arithmetically
		Pressure:       sysinfo.PressureCritical,
	}
	v := Assess(mem, &worktree.Inventory{}, baseCfg())
	if v.OK {
		t.Error("expected OK=false under critical pressure despite headroom")
	}
	if v.RoomForN != 0 {
		t.Errorf("RoomForN = %d, want 0 under critical pressure", v.RoomForN)
	}
	if v.PressureSticky {
		t.Error("expected PressureSticky=false under critical pressure")
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

func strictCfg() config.Config {
	c := baseCfg()
	c.StrictPressure = true
	return c
}

func TestAssess_StrictWarningGates(t *testing.T) {
	// Strict posture: warning hard-gates even with healthy headroom — but the
	// verdict still exposes what default WOULD allow, so the caller sees the cost.
	mem := sysinfo.Memory{
		TotalBytes:     24 << 30,
		AvailableBytes: 8 << 30, // 8 - 3 reserve = 5 usable, /1GB = 5
		Pressure:       sysinfo.PressureWarning,
	}
	v := Assess(mem, &worktree.Inventory{}, strictCfg())
	if v.OK {
		t.Error("strict: expected OK=false under warning")
	}
	if v.RoomForN != 0 {
		t.Errorf("RoomForN = %d, want 0 under strict warning", v.RoomForN)
	}
	if !v.Strict {
		t.Error("want Strict=true")
	}
	if v.HeadroomGatedRoom != 5 {
		t.Errorf("HeadroomGatedRoom = %d, want 5 (what default would allow)", v.HeadroomGatedRoom)
	}
	if v.PressureGatedRoom != 0 {
		t.Errorf("PressureGatedRoom = %d, want 0 under warning", v.PressureGatedRoom)
	}
}

func TestAssess_StrictNormalStillAllows(t *testing.T) {
	// Strict only affects warning; normal pressure still lets headroom decide.
	mem := sysinfo.Memory{
		TotalBytes:     24 << 30,
		AvailableBytes: 8 << 30,
		Pressure:       sysinfo.PressureNormal,
	}
	v := Assess(mem, &worktree.Inventory{}, strictCfg())
	if !v.OK || v.RoomForN != 5 {
		t.Errorf("strict+normal: OK=%v RoomForN=%d, want true/5", v.OK, v.RoomForN)
	}
	if v.PressureGatedRoom != 5 || v.HeadroomGatedRoom != 5 {
		t.Errorf("counts under normal: pg=%d hg=%d, want 5/5", v.PressureGatedRoom, v.HeadroomGatedRoom)
	}
}

func TestAssess_StrictCriticalStillGates(t *testing.T) {
	// Critical hard-gates in every posture — strict cannot make it worse, default
	// cannot make it pass.
	mem := sysinfo.Memory{
		TotalBytes:     24 << 30,
		AvailableBytes: 20 << 30,
		Pressure:       sysinfo.PressureCritical,
	}
	if v := Assess(mem, &worktree.Inventory{}, strictCfg()); v.OK || v.RoomForN != 0 {
		t.Error("critical must hard-gate under strict")
	}
}

func TestAssess_CountsExposedDefault(t *testing.T) {
	// Default (non-strict) under warning: room_for_n follows headroom, and the two
	// counts show both postures so a caller need not read raw bytes.
	mem := sysinfo.Memory{
		TotalBytes:     24 << 30,
		AvailableBytes: 8 << 30,
		Pressure:       sysinfo.PressureWarning,
	}
	v := Assess(mem, &worktree.Inventory{}, baseCfg())
	if v.RoomForN != 5 || v.HeadroomGatedRoom != 5 {
		t.Errorf("RoomForN=%d HeadroomGatedRoom=%d, want 5/5", v.RoomForN, v.HeadroomGatedRoom)
	}
	if v.PressureGatedRoom != 0 {
		t.Errorf("PressureGatedRoom = %d, want 0 (warning, cautious count)", v.PressureGatedRoom)
	}
	if v.Strict {
		t.Error("Strict should be false for baseCfg")
	}
}
