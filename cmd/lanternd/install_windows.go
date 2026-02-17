package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func install(dataPath, logPath, logLevel string) error {
	dataPath = os.ExpandEnv(dataPath)
	logPath = os.ExpandEnv(logPath)

	slog.Info("Installing Windows service..")
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to Windows service manager: %w", err)
	}

	if service, err := m.OpenService(serviceName); err == nil {
		service.Close()
		return fmt.Errorf("service %q is already installed", serviceName)
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}

	config := mgr.Config{
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
		DisplayName:  serviceName,
		Description:  "Lantern Daemon Service",
	}

	args := []string{
		"run",
		"--data-path", dataPath,
		"--log-path", logPath,
		"--log-level", logLevel,
	}

	slog.Info("Creating Windows service", "exe", exe, "args", args)
	service, err := m.CreateService(serviceName, exe, config, args...)
	if err != nil {
		return fmt.Errorf("failed to create %q service: %w", serviceName, err)
	}
	defer service.Close()

	err = service.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 1 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 2 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 4 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 8 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 16 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 32 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 64 * time.Second},
	}, 60)
	if err != nil {
		return fmt.Errorf("failed to set service recovery actions: %w", err)
	}
	if err := service.Start(); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}

	slog.Info("Windows service installed successfully")
	return nil
}

func uninstall() error {
	slog.Info("Uninstalling Windows service..")
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to Windows service manager: %w", err)
	}
	defer m.Disconnect()

	service, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("failed to open %q service: %w", serviceName, err)
	}

	status, err := service.Query()
	if err != nil {
		service.Close()
		return fmt.Errorf("failed to query service state: %w", err)
	}
	if status.State != svc.Stopped {
		service.Control(svc.Stop)
	}
	err = service.Delete()
	service.Close()
	if err != nil {
		return fmt.Errorf("failed to delete service: %w", err)
	}

	slog.Info("Waiting for service to be removed...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for service to be removed")
		case <-time.After(100 * time.Millisecond):
			if service, err = m.OpenService(serviceName); err != nil {
				slog.Info("Windows service uninstalled successfully")
				return nil
			}
			service.Close()
		}
	}
}
