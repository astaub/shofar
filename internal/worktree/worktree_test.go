package worktree

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestDiscover(t *testing.T) {
	base := t.TempDir()
	mk := func(rel string, gitIsDir bool) {
		dir := filepath.Join(base, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		g := filepath.Join(dir, ".git")
		if gitIsDir {
			os.MkdirAll(g, 0o755)
		} else {
			os.WriteFile(g, []byte("gitdir: /elsewhere"), 0o644)
		}
	}
	mk("wt1", false)                      // depth 1, linked worktree (.git FILE)
	mk("proj/wt2", true)                  // depth 2, clone (.git DIR)
	mk("proj/wt2/node_modules/dep", true) // inside a worktree → must NOT be found
	os.MkdirAll(filepath.Join(base, "proj", "empty"), 0o755) // no .git → not a worktree

	got := Discover([]string{base, "/no/such/base"})
	var names []string
	for _, w := range got {
		names = append(names, w.Name)
	}
	sort.Strings(names)

	want := []string{"wt1", "wt2"}
	if len(names) != len(want) {
		t.Fatalf("Discover found %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("Discover[%d] = %q, want %q (full: %v)", i, names[i], want[i], names)
		}
	}
}
