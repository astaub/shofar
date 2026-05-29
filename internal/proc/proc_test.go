package proc

import (
	"testing"
	"time"
)

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
