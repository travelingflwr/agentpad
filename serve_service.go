package main

// `agentpad serve install` keeps the bridge running as a service of your
// own user: a LaunchAgent on macOS, a systemd user unit on Linux. Both
// start it at login and restart it if it stops. `agentpad serve uninstall`
// takes it away again. Neither needs root.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	launchLabel = "com.travelingflwr.agentpad.serve"
	systemdUnit = "agentpad-serve.service"
)

func serveLogPath() string { return filepath.Join(stateDir(), "serve.log") }

func launchAgentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist")
}

func systemdUnitPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", systemdUnit)
}

func runServiceInstall(port int, remove bool) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, _ = filepath.EvalSymlinks(self)
	switch runtime.GOOS {
	case "darwin":
		return launchdService(self, port, remove)
	case "linux":
		return systemdService(self, port, remove)
	}
	return fmt.Errorf("no service support on %s: run agentpad serve yourself", runtime.GOOS)
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func launchdService(self string, port int, remove bool) error {
	plist := launchAgentPath()
	domain := "gui/" + strconv.Itoa(os.Getuid())
	exec.Command("launchctl", "bootout", domain+"/"+launchLabel).Run() // stop any running copy
	if remove {
		os.Remove(plist)
		fmt.Println("The bridge no longer runs as a service.")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	os.MkdirAll(stateDir(), 0o700)
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
		<string>--port</string>
		<string>%d</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, launchLabel, xmlEscape(self), port, xmlEscape(serveLogPath()), xmlEscape(serveLogPath()))
	if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("launchctl", "bootstrap", domain, plist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Println("The bridge now runs as a service: it starts at login and restarts if it stops.")
	fmt.Println("Log:", serveLogPath())
	return nil
}

func systemdService(self string, port int, remove bool) error {
	unit := systemdUnitPath()
	ctl := func(args ...string) error {
		out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if remove {
		ctl("disable", "--now", systemdUnit)
		os.Remove(unit)
		ctl("daemon-reload")
		fmt.Println("The bridge no longer runs as a service.")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf(`[Unit]
Description=agentpad serve: the bridge for the Nod iPhone remote
After=network-online.target

[Service]
ExecStart=%s serve --port %d
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`, self, port)
	if err := os.WriteFile(unit, []byte(body), 0o644); err != nil {
		return err
	}
	if err := ctl("daemon-reload"); err != nil {
		return err
	}
	if err := ctl("enable", "--now", systemdUnit); err != nil {
		return err
	}
	fmt.Println("The bridge now runs as a service. To keep it running while you're logged out:")
	fmt.Println("  loginctl enable-linger $USER")
	fmt.Println("Log: journalctl --user -u " + systemdUnit)
	return nil
}
