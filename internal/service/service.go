// Package service runs the daemon as a per-user system service.
package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

const (
	launchdLabel    = "dev.genail.cc-agent"
	systemdUnitName = "cc-agent.service"
	systemdRunDir   = "/run/systemd/system" // exists when systemd is the init system
	unitFileMode    = 0o600
)

// passedEnv are the variables the daemon must share with the user's sessions.
var passedEnv = []string{"CLAUDE_CONFIG_DIR", "CC_AGENT_HOME"}

// Install writes the service definition and starts it.
func Install(binary, logPath string) error {
	env := map[string]string{}
	for _, name := range passedEnv {
		if v := os.Getenv(name); v != "" {
			env[name] = v
		}
	}
	switch runtime.GOOS {
	case "darwin":
		path, err := launchdPath()
		if err != nil {
			return err
		}
		if err := write(path, launchdPlist(binary, logPath, env)); err != nil {
			return err
		}
		_ = exec.Command("launchctl", "bootout", launchdTarget()).Run() // fails when not loaded yet
		return run("launchctl", "bootstrap", launchdDomain(), path)
	case "linux":
		if err := requireSystemd(); err != nil {
			return err
		}
		path, err := systemdPath()
		if err != nil {
			return err
		}
		if err := write(path, systemdUnit(binary, env)); err != nil {
			return err
		}
		if err := run("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		if err := run("systemctl", "--user", "enable", systemdUnitName); err != nil {
			return err
		}
		return run("systemctl", "--user", "restart", systemdUnitName)
	}
	return unsupported()
}

// Uninstall stops the service and removes its definition.
func Uninstall() error {
	switch runtime.GOOS {
	case "darwin":
		path, err := launchdPath()
		if err != nil {
			return err
		}
		return errors.Join(run("launchctl", "bootout", launchdTarget()), os.Remove(path))
	case "linux":
		if err := requireSystemd(); err != nil {
			return err
		}
		path, err := systemdPath()
		if err != nil {
			return err
		}
		return errors.Join(run("systemctl", "--user", "disable", "--now", systemdUnitName), os.Remove(path),
			run("systemctl", "--user", "daemon-reload"))
	}
	return unsupported()
}

func launchdPlist(binary, logPath string, env map[string]string) string {
	var vars strings.Builder
	for _, name := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(&vars, "    <key>%s</key><string>%s</string>\n", xmlText(name), xmlText(env[name]))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array><string>%s</string><string>daemon</string></array>
  <key>EnvironmentVariables</key>
  <dict>
%s  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchdLabel, xmlText(binary), vars.String(), xmlText(logPath))
}

func systemdUnit(binary string, env map[string]string) string {
	var vars strings.Builder
	for _, name := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(&vars, "Environment=%s\n", systemdQuote(name+"="+env[name]))
	}
	return fmt.Sprintf(`[Unit]
Description=cc-agent relay between agent sessions

[Service]
ExecStart=%s daemon
%sRestart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, systemdQuote(binary), vars.String())
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s)) // a bytes.Buffer never fails
	return b.String()
}

func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s) + `"`
}

func launchdDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }
func launchdTarget() string { return launchdDomain() + "/" + launchdLabel }

func launchdPath() (string, error) {
	home, err := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), err
}

func systemdPath() (string, error) {
	dir, err := os.UserConfigDir()
	return filepath.Join(dir, "systemd", "user", systemdUnitName), err
}

func requireSystemd() error {
	if _, err := os.Stat(systemdRunDir); err != nil {
		return errors.New("systemd is not running. On WSL add [boot] systemd=true to /etc/wsl.conf and run " +
			"`wsl --shutdown` from Windows, or keep `cc-agent daemon` running in a terminal")
	}
	return nil
}

func write(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), unitFileMode)
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

func unsupported() error {
	return fmt.Errorf("services are not supported on %s; keep `cc-agent daemon` running in a terminal", runtime.GOOS)
}
