// Package proc enumerates running processes, classifies the ones relevant to a
// dev-worktree workflow (AI agent CLIs, dev servers, test runners), and resolves
// their working directory so they can be mapped to a worktree. It also exposes
// the self-ancestor guard: the set of PIDs in our own parent chain, which must
// never be killed (so running `shofar clean` from inside a Claude/Codex
// session can't terminate that session).
package proc

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Kind classifies a process by the role it plays in a dev workflow.
type Kind string

const (
	KindClaude      Kind = "claude"
	KindCodex       Kind = "codex"
	KindCursorAgent Kind = "cursor-agent"
	KindDevServer   Kind = "dev-server"
	KindTestRunner  Kind = "test-runner"
	KindOther       Kind = "other"
)

// Absolute paths to the macOS tools we shell out to — see sysinfo for the
// PATH-hijack rationale. A process-killing tool must not trust PATH for the
// commands that decide which PIDs to signal.
const (
	binPS   = "/bin/ps"
	binLsof = "/usr/sbin/lsof"
)

// Proc is a single process with the fields shofar reasons about.
type Proc struct {
	PID          int           `json:"pid"`
	PPID         int           `json:"ppid"`
	RSSBytes     uint64        `json:"rss_bytes"`
	Elapsed      time.Duration `json:"-"`
	TTY          string        `json:"tty"`
	Command      string        `json:"command"`
	Kind         Kind          `json:"kind"`
	Cwd          string        `json:"cwd,omitempty"`
	Worktree     string        `json:"worktree,omitempty"`      // display name
	WorktreePath string        `json:"worktree_path,omitempty"` // unique key (names can collide across bases)

	// CwdUnresolved is true when working-directory resolution was ATTEMPTED
	// for this process but failed (lsof error/permission), as opposed to
	// resolving to a path outside any worktree. The cleaner fails closed on
	// these: a process we can't place can't be proven safe to kill.
	CwdUnresolved bool `json:"-"`
}

// Snapshot is the full process table plus the self-ancestor set.
type Snapshot struct {
	Procs     []*Proc
	byPID     map[int]*Proc
	children  map[int][]*Proc
	ancestors map[int]bool
}

// Scan reads the process table via `ps` and classifies each entry. Working
// directories are NOT resolved here (that requires a per-PID lsof and is
// expensive); call ResolveCwds for the subset that matters.
func Scan() (*Snapshot, error) {
	// command= is last so embedded spaces are preserved. -ww prevents column
	// truncation of long command lines.
	out, err := exec.Command(binPS, "-axww", "-o", "pid=,ppid=,rss=,etime=,tty=,command=").Output()
	if err != nil {
		return nil, err
	}
	var procs []*Proc
	for _, line := range strings.Split(string(out), "\n") {
		p := parsePSLine(line)
		if p == nil {
			continue
		}
		procs = append(procs, p)
	}
	return NewSnapshot(procs, os.Getpid()), nil
}

// NewSnapshot builds a Snapshot from an explicit process list and the PID whose
// ancestor chain should be protected. Exposed so callers (and tests) can
// compose snapshots without shelling out to ps.
func NewSnapshot(procs []*Proc, selfPID int) *Snapshot {
	s := &Snapshot{
		Procs:    procs,
		byPID:    make(map[int]*Proc, len(procs)),
		children: make(map[int][]*Proc, len(procs)),
	}
	for _, p := range procs {
		s.byPID[p.PID] = p
	}
	for _, p := range procs {
		if p.PPID != p.PID { // defensive: never index a process as its own child
			s.children[p.PPID] = append(s.children[p.PPID], p)
		}
	}
	s.ancestors = computeAncestors(selfPID, s.byPID)
	return s
}

// SubtreeRSS returns the combined resident memory of pid and all of its
// descendants. Killing a process tears down its children too (a tmux server
// takes its panes, a launcher takes the agent it spawned), so the memory a
// kill actually reclaims is the whole subtree — not just the named process.
// This is why an orphaned 4 MB launcher can be worth killing: its child holds
// the real RAM.
func (s *Snapshot) SubtreeRSS(pid int) uint64 {
	var total uint64
	seen := map[int]bool{}
	var walk func(int)
	walk = func(id int) {
		if seen[id] {
			return // guard against PPID cycles
		}
		seen[id] = true
		if p, ok := s.byPID[id]; ok {
			total += p.RSSBytes
		}
		for _, c := range s.children[id] {
			walk(c.PID)
		}
	}
	walk(pid)
	return total
}

// Subtree returns pid and all of its descendant procs — the exact set a subtree
// kill terminates, and the set safety checks must clear before that kill.
func (s *Snapshot) Subtree(pid int) []*Proc {
	var out []*Proc
	seen := map[int]bool{}
	var walk func(int)
	walk = func(id int) {
		if seen[id] {
			return
		}
		seen[id] = true
		if p, ok := s.byPID[id]; ok {
			out = append(out, p)
		}
		for _, c := range s.children[id] {
			walk(c.PID)
		}
	}
	walk(pid)
	return out
}

// SubtreeMinTTYIdle returns the shortest TTY idle time across pid and all of its
// descendants, and whether any process in the subtree has a usable TTY at all.
//
// This is the liveness test for a detached session. A `tmux new-session -d ...
// claude` launcher is reparented to launchd with no TTY of its own — by itself
// it looks exactly like an abandoned orphan. But its child claude holds the
// real interactive TTY. By taking the minimum idle across the whole tree we ask
// the right question: "has ANYONE in this session touched a terminal recently?"
// hasTTY=false means nothing in the tree has a terminal — only then is a
// reparented session a true orphan.
func (s *Snapshot) SubtreeMinTTYIdle(pid int, now time.Time) (idle time.Duration, hasTTY bool) {
	min := time.Duration(-1)
	seen := map[int]bool{}
	var walk func(int)
	walk = func(id int) {
		if seen[id] {
			return
		}
		seen[id] = true
		if p, ok := s.byPID[id]; ok {
			if d, ok2 := ttyIdle(p, now); ok2 {
				hasTTY = true
				if min < 0 || d < min {
					min = d
				}
			}
		}
		for _, c := range s.children[id] {
			walk(c.PID)
		}
	}
	walk(pid)
	if !hasTTY {
		return 0, false
	}
	return min, true
}

// parsePSLine parses one line of `ps -o pid=,ppid=,rss=,etime=,tty=,command=`.
// The first five fields are fixed; everything after is the command.
func parsePSLine(line string) *Proc {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	fields := strings.Fields(line)
	if len(fields) < 6 {
		return nil
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return nil
	}
	ppid, _ := strconv.Atoi(fields[1])
	rssKB, _ := strconv.ParseUint(fields[2], 10, 64)
	elapsed := ParseEtime(fields[3])
	tty := fields[4]
	// Reconstruct the command: skip the 5 fixed columns. Use the original line
	// to preserve internal spacing rather than re-joining split fields.
	command := commandTail(line, fields[:5])

	p := &Proc{
		PID:      pid,
		PPID:     ppid,
		RSSBytes: rssKB * 1024,
		Elapsed:  elapsed,
		TTY:      tty,
		Command:  command,
	}
	p.Kind = classify(command)
	return p
}

// commandTail returns the remainder of line after the fixed-width prefix
// columns. We locate the command by walking past the known leading fields.
func commandTail(line string, leading []string) string {
	rest := strings.TrimLeft(line, " ")
	for _, f := range leading {
		idx := strings.Index(rest, f)
		if idx < 0 {
			continue
		}
		rest = strings.TrimLeft(rest[idx+len(f):], " ")
	}
	return rest
}

// classify maps a command line to a Kind using substring heuristics tuned for
// macOS dev workflows.
func classify(command string) Kind {
	c := strings.ToLower(command)
	switch {
	case strings.Contains(c, "cursor-agent"):
		return KindCursorAgent
	case containsWord(c, "claude"):
		return KindClaude
	case containsWord(c, "codex"):
		return KindCodex
	case strings.Contains(c, "vitest") || strings.Contains(c, "jest"):
		return KindTestRunner
	case strings.Contains(c, "next dev") || strings.Contains(c, "next-server") ||
		strings.Contains(c, "vite") || strings.Contains(c, "pnpm dev") ||
		strings.Contains(c, "npm run dev") || strings.Contains(c, "yarn dev") ||
		strings.Contains(c, "puma") || strings.Contains(c, "overmind") ||
		strings.Contains(c, "esbuild"):
		return KindDevServer
	default:
		return KindOther
	}
}

// containsWord reports whether needle appears in s delimited by non-word
// characters, so "claude" matches "/usr/bin/claude --foo" but not "claudette".
func containsWord(s, needle string) bool {
	idx := 0
	for {
		i := strings.Index(s[idx:], needle)
		if i < 0 {
			return false
		}
		start := idx + i
		end := start + len(needle)
		leftOK := start == 0 || !isWordByte(s[start-1])
		rightOK := end == len(s) || !isWordByte(s[end])
		if leftOK && rightOK {
			return true
		}
		idx = end
	}
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// ParseEtime parses `ps` elapsed-time format ([[DD-]HH:]MM:SS) to a Duration.
func ParseEtime(etime string) time.Duration {
	if etime == "" {
		return 0
	}
	var days int
	if i := strings.Index(etime, "-"); i >= 0 {
		days, _ = strconv.Atoi(etime[:i])
		etime = etime[i+1:]
	}
	parts := strings.Split(etime, ":")
	var h, m, sec int
	switch len(parts) {
	case 2:
		m, _ = strconv.Atoi(parts[0])
		sec, _ = strconv.Atoi(parts[1])
	case 3:
		h, _ = strconv.Atoi(parts[0])
		m, _ = strconv.Atoi(parts[1])
		sec, _ = strconv.Atoi(parts[2])
	default:
		return 0
	}
	return time.Duration(days)*24*time.Hour +
		time.Duration(h)*time.Hour +
		time.Duration(m)*time.Minute +
		time.Duration(sec)*time.Second
}

// IsSelfAncestor reports whether pid is in our own parent chain.
func (s *Snapshot) IsSelfAncestor(pid int) bool { return s.ancestors[pid] }

// LookupPID returns the Proc for a given PID, if present in the snapshot.
func (s *Snapshot) LookupPID(pid int) (*Proc, bool) {
	p, ok := s.byPID[pid]
	return p, ok
}

// computeAncestors walks the PPID chain from start up to the root.
func computeAncestors(start int, byPID map[int]*Proc) map[int]bool {
	anc := map[int]bool{}
	p := start
	for p > 1 {
		anc[p] = true
		proc, ok := byPID[p]
		if !ok {
			break
		}
		if proc.PPID == p { // defensive: avoid self-loop
			break
		}
		p = proc.PPID
	}
	return anc
}

// ResolveCwds fills in Cwd for the given procs using a single `lsof` per PID.
// lsof is expensive, so callers pass only the candidates they care about. When
// lsof errors, CwdUnresolved is set so downstream safety checks can fail closed.
func (s *Snapshot) ResolveCwds(procs []*Proc) {
	for _, p := range procs {
		if p.Cwd != "" {
			continue
		}
		cwd, ok := lsofCwd(p.PID)
		if !ok {
			p.CwdUnresolved = true
			continue
		}
		p.Cwd = cwd
	}
}

// lsofCwd returns the working directory of pid. ok is false when lsof itself
// errored (the cwd is unknown), distinct from a successful lookup that simply
// found no path.
func lsofCwd(pid int) (cwd string, ok bool) {
	out, err := exec.Command(binLsof, "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n/") {
			return strings.TrimPrefix(line, "n"), true
		}
	}
	return "", true
}

// Verify re-reads the process table for p.PID and reports whether it still looks
// like the same process (same command and a plausibly-longer elapsed time).
// Used immediately before sending a kill signal to defend against PID reuse
// between the original scan and the kill.
func Verify(p *Proc) bool {
	out, err := exec.Command(binPS, "-p", strconv.Itoa(p.PID), "-o", "etime=,command=").Output()
	if err != nil {
		return false // process gone or unreadable — do not signal
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return false
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return false
	}
	nowElapsed := ParseEtime(fields[0])
	command := commandTail(line, fields[:1])
	// Command must match exactly, and the process must be at least as old as
	// when we scanned it (a reused PID would be younger).
	return command == p.Command && nowElapsed+time.Second >= p.Elapsed
}

// ttyIdle is the TTY-idle lookup used by SubtreeMinTTYIdle. It indirects through
// a package var so tests can inject deterministic idle values without touching
// real /dev devices.
var ttyIdle = TTYIdle

// TTYIdle returns how long the process's controlling TTY has been idle (no
// keystroke or program output), or ok=false if it has no usable TTY. The TTY
// device's mtime advances on any I/O, mirroring the bash implementation.
func TTYIdle(p *Proc, now time.Time) (idle time.Duration, ok bool) {
	if p.TTY == "" || p.TTY == "??" {
		return 0, false
	}
	dev := "/dev/" + p.TTY
	info, err := os.Stat(dev)
	if err != nil {
		return 0, false
	}
	return now.Sub(info.ModTime()), true
}
