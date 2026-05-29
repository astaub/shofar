package config

import "testing"

func TestValidate(t *testing.T) {
	ok := Default()
	if err := ok.Validate(); err != nil {
		t.Errorf("default config should validate: %v", err)
	}

	neg := Default()
	neg.MinSessionMinutes = -1
	if err := neg.Validate(); err == nil {
		t.Error("negative MinSessionMinutes must be rejected")
	}

	zero := Default()
	zero.DefaultWorktreeBudgetBytes = 0
	if err := zero.Validate(); err == nil {
		t.Error("zero default budget must be rejected (disables capacity math)")
	}
}

func TestExpandTilde(t *testing.T) {
	h := home()
	if got := expandTilde("~/code/worktrees"); got != h+"/code/worktrees" {
		t.Errorf("expandTilde(~/code/worktrees) = %q, want %q", got, h+"/code/worktrees")
	}
	if got := expandTilde("/abs/path"); got != "/abs/path" {
		t.Errorf("expandTilde should leave absolute paths unchanged, got %q", got)
	}
}
