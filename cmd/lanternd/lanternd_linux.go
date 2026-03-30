package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"text/template"
)

const (
	serviceName     = "lanternd"
	defaultDataPath = "/var/lib/lantern"
	defaultLogPath  = "/var/log/lantern"
	binPath         = "/usr/bin/" + serviceName
)

func maybePlatformService() bool {
	return false
}

var systemdUnitTmpl = template.Must(template.New("unit").Parse(`[Unit]
Description=Lantern VPN Daemon
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
ExecStart={{.ExePath}} run --data-path {{.DataPath}} --log-path {{.LogPath}} --log-level {{.LogLevel}}
Restart=on-failure
RestartSec=5s

RuntimeDirectory=lantern
RuntimeDirectoryMode=0755
StateDirectory=lantern
CacheDirectory=lantern
LogsDirectory=lantern

[Install]
WantedBy=multi-user.target
`))

func install(dataPath, logPath, logLevel string) error {
	if err := checkInstalledVersion(); err != nil {
		return err
	}
	exe, err := copyBin()
	if err != nil {
		return err
	}

	unitPath := fmt.Sprintf("/etc/systemd/system/%s.service", serviceName)
	f, err := os.Create(unitPath)
	if err != nil {
		return fmt.Errorf("failed to create unit file %s: %w", unitPath, err)
	}
	defer f.Close()

	err = systemdUnitTmpl.Execute(f, struct {
		ExePath, DataPath, LogPath, LogLevel string
	}{exe, dataPath, logPath, logLevel})
	if err != nil {
		return fmt.Errorf("failed to write unit file: %w", err)
	}

	slog.Info("Installing systemd service", "unit", unitPath)
	for _, args := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", serviceName},
		{"systemctl", "start", serviceName},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %w\n%s", args, err, out)
		}
	}

	slog.Info("Systemd service installed and started")
	return nil
}

func uninstall() error {
	slog.Info("Uninstalling systemd service")
	for _, args := range [][]string{
		{"systemctl", "stop", serviceName},
		{"systemctl", "disable", serviceName},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			slog.Warn("Command failed", "cmd", args, "error", err, "output", string(out))
		}
	}

	unitPath := fmt.Sprintf("/etc/systemd/system/%s.service", serviceName)
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove unit file: %w", err)
	}

	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w\n%s", err, out)
	}

	slog.Info("Systemd service uninstalled")
	if err := os.Remove(binPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove binary: %w", err)
	}
	return nil
}
