// Package launchd manages the macOS LaunchAgent that runs `shofar clean
// --kill` on a schedule. Turning cleanup on installs and bootstraps the agent;
// turning it off boots it out and removes the plist. The agent fires hourly and
// at login (RunAtLoad), so it re-arms itself after a reboot.
package launchd

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

const Label = "com.shofar.cleanup"

// PlistPath returns the LaunchAgent plist location for the current user.
func PlistPath() string {
	return filepath.Join(home(), "Library", "LaunchAgents", Label+".plist")
}

// Status reports whether the agent is currently loaded in launchd.
func Status() (loaded bool) {
	// `launchctl list <label>` exits non-zero when the label is not loaded.
	return exec.Command("launchctl", "list", Label).Run() == nil
}

// Install writes the plist (pointing at the given shofar binary) and
// bootstraps it into the user's GUI domain. It is idempotent: an already-loaded
// agent is booted out first so the new definition takes effect.
func Install(binPath string, intervalSeconds int) error {
	logPath := filepath.Join(home(), ".local", "log", "shofar-cleanup.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	plist := renderPlist(binPath, logPath, intervalSeconds)

	p := PlistPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(plist), 0o644); err != nil {
		return err
	}

	// Reload cleanly: bootout (ignore "not loaded" errors), then bootstrap.
	domain := guiDomain()
	_ = exec.Command("launchctl", "bootout", domain+"/"+Label).Run()
	if out, err := exec.Command("launchctl", "bootstrap", domain, p).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	return nil
}

// Uninstall boots the agent out and removes the plist. It verifies the agent is
// actually unloaded before reporting success, so callers never tell the user
// cleanup is off while a loaded agent keeps firing. Missing artifacts are not an
// error.
func Uninstall() error {
	// bootout returns non-zero when the label isn't loaded; that's fine. We
	// confirm the real outcome via Status() rather than trusting the exit code.
	_ = exec.Command("launchctl", "bootout", guiDomain()+"/"+Label).Run()
	if Status() {
		return fmt.Errorf("launchctl bootout did not unload %s; it may still be running", Label)
	}
	if err := os.Remove(PlistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func guiDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func renderPlist(binPath, logPath string, intervalSeconds int) string {
	binPath = xmlEscape(binPath)
	logPath = xmlEscape(logPath)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>clean</string>
		<string>--kill</string>
	</array>
	<key>StartInterval</key>
	<integer>%d</integer>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
</dict>
</plist>
`, Label, binPath, intervalSeconds, logPath, logPath)
}

// xmlEscape escapes a string for safe inclusion in plist XML character data,
// so a path containing &, <, >, or quotes can't break or inject into the plist.
func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}
