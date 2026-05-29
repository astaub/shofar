package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// skillMD is the Agent Skill playbook, embedded so the binary can install it
// into any agent's skills directory without a separate download.
//
//go:embed skills/shofar/SKILL.md
var skillMD string

// knownAgentDirs maps an agent name to its user-scope skills directory. Only
// agents whose path is well-established are listed; everything else uses --dir.
// The Agent Skills SKILL.md format is shared across tools, but each tool reads
// from a different directory, so installation is per-agent.
func knownAgentDirs() map[string]string {
	h := homeDir()
	return map[string]string{
		"claude": filepath.Join(h, ".claude", "skills"),
	}
}

func cmdSkill(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: shofar skill print|path|install [--agent <name>] [--dir <path>]")
	}
	switch args[0] {
	case "print":
		fmt.Print(skillMD)
		return nil
	case "path":
		return skillPath()
	case "install":
		return skillInstall(args[1:])
	default:
		return fmt.Errorf("usage: shofar skill print|path|install [--agent <name>] [--dir <path>]")
	}
}

func skillPath() error {
	fmt.Println("Known agent skills directories:")
	for name, dir := range knownAgentDirs() {
		fmt.Printf("  %-8s %s\n", name, dir)
	}
	fmt.Println("\nFor any other agent, install with --dir <that agent's skills directory>,")
	fmt.Println("or pipe `shofar skill print` into the file the agent expects.")
	return nil
}

func skillInstall(args []string) error {
	agent := flagValue(args, "--agent")
	dir := flagValue(args, "--dir")

	target := dir
	if target == "" {
		if agent == "" {
			return fmt.Errorf("specify --agent <name> or --dir <path> (see `shofar skill path`)")
		}
		known := knownAgentDirs()
		d, ok := known[agent]
		if !ok {
			return fmt.Errorf("unknown agent %q; use --dir <path> instead (see `shofar skill path`)", agent)
		}
		target = d
	}

	skillDir := filepath.Join(target, "shofar")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(dest, []byte(skillMD), 0o644); err != nil {
		return err
	}
	fmt.Printf("Installed shofar skill -> %s\n", dest)
	return nil
}

// flagValue returns the value following flag in args (--flag value), or "".
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}
