//go:build darwin && !ios

package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"text/template"
)

const (
	serviceName     = "com.lantern.lanternd"
	defaultDataPath = "/Library/Application Support/Lantern"
	defaultLogPath  = "/Library/Logs/Lantern"
	binPath         = "/usr/local/bin/" + serviceName
)

func maybePlatformService() bool {
	return false
}

var launchdPlistTmpl = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.ServiceName}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.ExePath}}</string>
		<string>run</string>
		<string>--data-path</string>
		<string>{{.DataPath}}</string>
		<string>--log-path</string>
		<string>{{.LogPath}}</string>
		<string>--log-level</string>
		<string>{{.LogLevel}}</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>{{.LogPath}}/lanternd.stdout.log</string>
	<key>StandardErrorPath</key>
	<string>{{.LogPath}}/lanternd.stderr.log</string>
</dict>
</plist>
`))

func plistPath() string {
	return fmt.Sprintf("/Library/LaunchDaemons/%s.plist", serviceName)
}

func install(dataPath, logPath, logLevel string) error {
	if err := checkInstalledVersion(); err != nil {
		return err
	}
	exe, err := copyBin()
	if err != nil {
		return err
	}

	plist := plistPath()
	f, err := os.Create(plist)
	if err != nil {
		return fmt.Errorf("failed to create plist %s: %w", plist, err)
	}
	defer f.Close()

	err = launchdPlistTmpl.Execute(f, struct {
		ServiceName, ExePath, DataPath, LogPath, LogLevel string
	}{serviceName, exe, dataPath, logPath, logLevel})
	if err != nil {
		return fmt.Errorf("failed to write plist: %w", err)
	}

	slog.Info("Installing launchd service", "plist", plist)
	if out, err := exec.Command("launchctl", "load", "-w", plist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %w\n%s", err, out)
	}

	slog.Info("Launchd service installed and started")
	return nil
}

func uninstall() error {
	slog.Info("Uninstalling launchd service")
	plist := plistPath()

	if out, err := exec.Command("launchctl", "unload", "-w", plist).CombinedOutput(); err != nil {
		slog.Warn("Failed to unload service", "error", err, "output", string(out))
	}

	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove plist: %w", err)
	}

	slog.Info("Launchd service uninstalled")
	if err := os.Remove(binPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove binary: %w", err)
	}
	return nil
}
