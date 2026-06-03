package main

import (
	"fmt"

	"github.com/astaub/shofar/internal/cleaner"
)

func cmdClean(args []string) error {
	jsonOut := hasFlag(args, "--json")
	doKill := hasFlag(args, "--kill")

	s, err := newScan()
	if err != nil {
		return err
	}
	cands := cleaner.Select(s.cfg, s.snap, s.inv, s.now)

	if jsonOut {
		type result struct {
			DryRun     bool                `json:"dry_run"`
			Candidates []cleaner.Candidate `json:"candidates"`
			Killed     []int               `json:"killed,omitempty"`
			Skipped    []int               `json:"skipped,omitempty"`
			Errors     map[int]string      `json:"errors,omitempty"`
		}
		res := result{DryRun: !doKill, Candidates: cands}
		var killErr error
		if doKill {
			killed, skipped, errs := cleaner.Kill(cands)
			res.Killed = killed
			res.Skipped = skipped
			if len(errs) > 0 {
				res.Errors = map[int]string{}
				for pid, e := range errs {
					res.Errors[pid] = e.Error()
				}
				killErr = fmt.Errorf("%d process(es) failed to terminate", len(errs))
			}
		}
		if err := emitJSON(res); err != nil {
			return err
		}
		return killErr // non-zero exit so launchd/automation sees the failure
	}

	if len(cands) == 0 {
		fmt.Println("No safe-to-kill stale processes found.")
		return nil
	}

	var reclaim uint64
	for _, c := range cands {
		reclaim += c.ReclaimBytes
	}
	verb := "Would kill"
	if doKill {
		verb = "Killing"
	}
	fmt.Printf("%s %d process(es), reclaiming ~%s:\n\n", verb, len(cands), fmtBytes(reclaim))
	for _, c := range cands {
		wt := c.Proc.Worktree
		if wt == "" {
			wt = "-"
		}
		// Show subtree reclaim; append the bare process RSS in parens when the
		// child tree holds materially more than the named process (e.g. a tiny
		// orphan launcher whose agent child holds the real memory).
		size := fmtBytes(c.ReclaimBytes)
		if c.ReclaimBytes >= c.Proc.RSSBytes*2 && c.ReclaimBytes-c.Proc.RSSBytes > 50<<20 {
			size = fmt.Sprintf("%s (proc %s + subtree)", fmtBytes(c.ReclaimBytes), fmtBytes(c.Proc.RSSBytes))
		}
		fmt.Printf("  pid %-7d %-12s %-26s  %-22s  %s\n",
			c.Proc.PID, c.Proc.Kind, size, wt, c.Reason)
	}

	if !doKill {
		fmt.Println("\nDry run. Re-run with --kill to terminate these processes.")
		return nil
	}

	killed, skipped, errs := cleaner.Kill(cands)
	fmt.Printf("\nSent SIGTERM to %d process(es).\n", len(killed))
	if len(skipped) > 0 {
		fmt.Printf("Skipped %d (exited or PID reused since scan).\n", len(skipped))
	}
	for pid, e := range errs {
		fmt.Printf("  pid %d: %v\n", pid, e)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d process(es) failed to terminate", len(errs))
	}
	return nil
}
