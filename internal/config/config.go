// Package config loads and persists shofar's settings from
// ~/.config/shofar/config.json. Every field has a sane default so the tool
// works with no config file present; the file only needs to exist to override a
// default or to record the cleanup on/off toggle.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds all tunables. Durations are stored as whole minutes/hours for
// human-editability.
type Config struct {
	// WorktreeBases are directories whose immediate subdirectories are treated
	// as worktrees.
	WorktreeBases []string `json:"worktree_bases"`
	// ActiveSubdirs are paths within a worktree checked for recent edits as an
	// activity signal.
	ActiveSubdirs []string `json:"active_subdirs"`
	// ActiveMinutes: a worktree edited within this window counts as active.
	ActiveMinutes int `json:"active_minutes"`
	// ClaudeIdleHours: claude/codex sessions whose TTY has been idle this long
	// are eligible for cleanup.
	ClaudeIdleHours int `json:"claude_idle_hours"`
	// MinSessionMinutes: never touch a session younger than this.
	MinSessionMinutes int `json:"min_session_minutes"`
	// StaleAgentMinutes: cursor-agent processes older than this are eligible.
	StaleAgentMinutes int `json:"stale_agent_minutes"`
	// ReserveBytes is physical memory held back for the OS and foreground apps
	// when computing capacity headroom.
	ReserveBytes uint64 `json:"reserve_bytes"`
	// DefaultWorktreeBudgetBytes is the per-worktree RAM estimate used when no
	// live worktrees exist to measure.
	DefaultWorktreeBudgetBytes uint64 `json:"default_worktree_budget_bytes"`
	// ProtectPatterns: any process whose command matches one of these substrings
	// is never killed.
	ProtectPatterns []string `json:"protect_patterns"`
	// CleanupEnabled records whether scheduled auto-cleanup is turned on.
	CleanupEnabled bool `json:"cleanup_enabled"`
	// StrictPressure makes the capacity gate treat WARNING memory pressure as a
	// hard block (room_for_n = 0), like critical does — the cautious posture for
	// a shared machine doing other heavy work. Default false: warning lets the
	// usable-headroom arithmetic decide (sticky swap/compression does not block
	// when genuinely free memory is healthy). The --strict flag forces this on
	// for a single invocation. Critical/unknown pressure always hard-gate
	// regardless of this setting.
	StrictPressure bool `json:"strict_pressure"`
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		WorktreeBases: []string{
			// Common roots for git worktrees and agent-harness trees. Discovery
			// walks each recursively, so nested layouts (emdash/cursor) are found;
			// non-existent roots are skipped. Add machine-specific roots via config.
			filepath.Join(home(), "code", "worktrees"),
			filepath.Join(home(), "emdash", "worktrees"),
			filepath.Join(home(), ".cursor", "worktrees"),
			filepath.Join(home(), "worktrees"),
		},
		ActiveSubdirs:              []string{"app", "config", "src", "lib", "apps", "packages", "cmd", "internal", "pkg"},
		ActiveMinutes:              1440, // 24h
		ClaudeIdleHours:            6,
		MinSessionMinutes:          30,
		StaleAgentMinutes:          120,
		ReserveBytes:               3 << 30, // 3 GiB held back for the OS + foreground apps
		DefaultWorktreeBudgetBytes: 1500 << 20,
		ProtectPatterns:            []string{},
		CleanupEnabled:             false,
	}
}

// Path returns the config file location.
func Path() string {
	return filepath.Join(home(), ".config", "shofar", "config.json")
}

// Load reads the config file, falling back to defaults for any missing file or
// unset field. A malformed file is a hard error so the user can fix it rather
// than silently running with surprising defaults.
func Load() (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	// Decode over the defaults so absent keys keep their default values.
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	// Expand a leading ~ in worktree base paths so a hand-written config like
	// "~/code/worktrees" resolves instead of silently matching nothing.
	for i, p := range cfg.WorktreeBases {
		cfg.WorktreeBases[i] = expandTilde(p)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid config at %s: %w", Path(), err)
	}
	return cfg, nil
}

// Validate rejects configurations that would make cleanup unsafe (negative
// thresholds turn every process eligible) or capacity meaningless (a zero
// per-worktree budget disables the arithmetic).
func (c Config) Validate() error {
	if c.ActiveMinutes < 0 || c.ClaudeIdleHours < 0 ||
		c.MinSessionMinutes < 0 || c.StaleAgentMinutes < 0 {
		return fmt.Errorf("time thresholds must not be negative")
	}
	if c.DefaultWorktreeBudgetBytes == 0 {
		return fmt.Errorf("default_worktree_budget_bytes must be > 0")
	}
	return nil
}

func expandTilde(p string) string {
	if p == "~" {
		return home()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home(), p[2:])
	}
	return p
}

// Save writes the config file, creating the directory if needed.
func (c Config) Save() error {
	p := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(p, data, 0o644)
}

func home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}
