package main

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/astaub/shofar/internal/cdp"
)

// Default DevTools endpoint shofar looks for (a debug Chrome — see `shofar
// chrome`). Shared so `status` can opportunistically fold in per-tab memory.
const (
	defaultCDPHost = "127.0.0.1"
	defaultCDPPort = 9222
)

// cmdChrome reports per-tab memory by talking to Chrome's DevTools Protocol.
// Per-tab memory isn't available from the OS process table (a renderer's
// command line has no URL), so this is the only way to attribute memory to a
// specific tab — at the cost of Chrome running with remote debugging enabled.
func cmdChrome(args []string) error {
	jsonOut := hasFlag(args, "--json")
	port := defaultCDPPort
	for i, a := range args {
		if a == "--port" && i+1 < len(args) {
			if p, err := strconv.Atoi(args[i+1]); err == nil {
				port = p
			}
		}
	}
	const host = defaultCDPHost

	if !cdp.Available(host, port) {
		if jsonOut {
			return emitJSON(map[string]any{"available": false, "port": port})
		}
		printChromeSetup(port)
		return nil
	}

	tabs, err := cdp.Tabs(host, port)
	if err != nil {
		return fmt.Errorf("query Chrome DevTools: %w", err)
	}
	sort.Slice(tabs, func(i, j int) bool { return tabs[i].JSHeapBytes > tabs[j].JSHeapBytes })

	if jsonOut {
		return emitJSON(map[string]any{"available": true, "port": port, "tabs": tabs})
	}

	if len(tabs) == 0 {
		fmt.Printf("Connected to Chrome on port %d, but no page tabs are open.\n", port)
		return nil
	}

	var total uint64
	for _, t := range tabs {
		total += t.JSHeapBytes
	}
	const nameW = 48
	fmt.Printf("Chrome tabs by memory  (JS heap, via DevTools port %d)\n", port)
	fmt.Printf("  %s  %10s\n", padRight("Tab", nameW), "JS HEAP")
	fmt.Printf("  %s  %10s\n", strings.Repeat("─", nameW), "──────────")
	for _, t := range tabs {
		label := t.Title
		if label == "" {
			label = t.URL
		}
		if h := hostOf(t.URL); h != "" {
			label = truncRunes(label, nameW-len(h)-3) + " — " + h
		}
		fmt.Printf("  %s  %10s\n", padRight(truncRunes(label, nameW), nameW), fmtBytes(t.JSHeapBytes))
	}
	fmt.Printf("  %s  %10s\n", padRight(fmt.Sprintf("%d tabs", len(tabs)), nameW), fmtBytes(total))
	fmt.Println("\nNote: JS-heap memory (a DevTools metric), not full process RSS — it ranks")
	fmt.Println("which tabs are heaviest. For total RSS, see `shofar status` Chrome breakdown.")
	return nil
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}

func printChromeSetup(port int) {
	fmt.Printf(`Chrome isn't reachable on DevTools port %d.

`+"`shofar chrome`"+` reads per-tab memory over Chrome's DevTools Protocol, which
must be enabled. Since Chrome 136 the DEFAULT profile can't be debugged (a
security measure), so run a SEPARATE debug Chrome alongside your normal one:

  1. Launch a second Chrome with a debug profile + port. Your everyday Chrome
     keeps running untouched (this is an independent instance + profile):

     "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
       --remote-debugging-port=%d \
       --user-data-dir="$HOME/.shofar-chrome-debug" &

  2. In the new window, sign in and open the tabs you want to measure.
  3. Re-run: shofar chrome

Security note: any local program can drive a debug-enabled Chrome (read cookies,
act as you). This uses a throwaway profile, not your everyday browser, to limit
that exposure.
`, port, port)
}
