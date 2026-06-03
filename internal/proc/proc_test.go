package proc

import (
	"testing"
	"time"
)

// TestSubtreeMinTTYIdle covers the detached-session liveness logic with an
// injected TTY-idle function (no real /dev access). The @architect bug: a
// reparented no-TTY launcher whose child holds a LIVE TTY must read as live.
func TestSubtreeMinTTYIdle(t *testing.T) {
	const mb = 1 << 20
	// launcher: reparented, no TTY of its own. child: the real session.
	launcher := &Proc{PID: 100, PPID: 1, Kind: KindClaude, TTY: "??", RSSBytes: 4 * mb}
	child := &Proc{PID: 101, PPID: 100, Kind: KindClaude, TTY: "ttys9", RSSBytes: 741 * mb}
	snap := NewSnapshot([]*Proc{launcher, child}, 99999)

	now := time.Unix(1_000_000, 0)
	orig := ttyIdle
	defer func() { ttyIdle = orig }()

	// Case 1 — child TTY active 2m ago: the whole session is LIVE even though
	// the launcher has no TTY. hasTTY must be true and idle small.
	ttyIdle = func(p *Proc, _ time.Time) (time.Duration, bool) {
		if p.PID == 101 {
			return 2 * time.Minute, true
		}
		return 0, false // launcher: no usable TTY
	}
	if idle, hasTTY := snap.SubtreeMinTTYIdle(100, now); !hasTTY || idle != 2*time.Minute {
		t.Errorf("live child: got (idle=%v, hasTTY=%v), want (2m, true)", idle, hasTTY)
	}

	// Case 2 — nothing in the tree has a TTY: a true orphan signal.
	ttyIdle = func(_ *Proc, _ time.Time) (time.Duration, bool) { return 0, false }
	if _, hasTTY := snap.SubtreeMinTTYIdle(100, now); hasTTY {
		t.Error("no-TTY tree: hasTTY should be false")
	}

	// Case 3 — child TTY idle 25h: session is abandoned-idle, min idle reported.
	ttyIdle = func(p *Proc, _ time.Time) (time.Duration, bool) {
		if p.PID == 101 {
			return 25 * time.Hour, true
		}
		return 0, false
	}
	if idle, hasTTY := snap.SubtreeMinTTYIdle(100, now); !hasTTY || idle != 25*time.Hour {
		t.Errorf("idle child: got (idle=%v, hasTTY=%v), want (25h, true)", idle, hasTTY)
	}
}

func TestParseEtime(t *testing.T) {
	cases := map[string]time.Duration{
		"05:30":      5*time.Minute + 30*time.Second,
		"01:05:30":   time.Hour + 5*time.Minute + 30*time.Second,
		"2-03:00:00": 2*24*time.Hour + 3*time.Hour,
		"":           0,
	}
	for in, want := range cases {
		if got := ParseEtime(in); got != want {
			t.Errorf("ParseEtime(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		command string
		want    Kind
	}{
		{"/Users/x/.local/bin/claude --resume", KindClaude},
		{"node /opt/codex/bin/codex", KindCodex},
		{"node .../cursor-agent/index.js", KindCursorAgent},
		{"node next dev --port 3000", KindDevServer},
		{"puma 6.0 [myapp]", KindDevServer},
		{"node vitest run", KindTestRunner},
		{"/bin/zsh -l", KindOther},
		{"claudette-helper", KindOther}, // word-boundary: not "claude"
	}
	for _, c := range cases {
		if got := classify(c.command); got != c.want {
			t.Errorf("classify(%q) = %q, want %q", c.command, got, c.want)
		}
	}
}

func TestParsePSLine(t *testing.T) {
	// pid ppid rss etime tty command...
	line := "  1234   567  204800 01:05:30 ttys003 /Users/x/.local/bin/claude --resume foo"
	p := parsePSLine(line)
	if p == nil {
		t.Fatal("parsePSLine returned nil")
	}
	if p.PID != 1234 || p.PPID != 567 {
		t.Errorf("pid/ppid = %d/%d, want 1234/567", p.PID, p.PPID)
	}
	if p.RSSBytes != 204800*1024 {
		t.Errorf("rss = %d, want %d", p.RSSBytes, 204800*1024)
	}
	if p.TTY != "ttys003" {
		t.Errorf("tty = %q, want ttys003", p.TTY)
	}
	if p.Kind != KindClaude {
		t.Errorf("kind = %q, want claude", p.Kind)
	}
	if p.Command != "/Users/x/.local/bin/claude --resume foo" {
		t.Errorf("command = %q", p.Command)
	}
}

func TestComputeAncestors(t *testing.T) {
	byPID := map[int]*Proc{
		100: {PID: 100, PPID: 50},
		50:  {PID: 50, PPID: 1},
		200: {PID: 200, PPID: 1},
	}
	anc := computeAncestors(100, byPID)
	if !anc[100] || !anc[50] {
		t.Errorf("expected 100 and 50 to be ancestors: %v", anc)
	}
	if anc[200] {
		t.Errorf("200 should not be an ancestor")
	}
}
