package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName     = "lanternd"
	defaultDataPath = "$PROGRAMDATA\\lantern"
	defaultLogPath  = "$PROGRAMDATA\\lantern"
	binPath         = "C:\\Program Files\\Lantern\\" + serviceName + ".exe"
)

var isWindowsService bool

func init() {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		log.Fatalf("Failed to determine if running as Windows service: %v\n", err)
	}
	isWindowsService = isSvc
}

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

	exe, err := copyBin()
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

func maybePlatformService() bool {
	if !isWindowsService {
		return false
	}
	if err := startWindowsService(); err != nil {
		log.Fatalf("Failed to start Windows service: %v\n", err)
	}
	return true
}

type service struct{}

func startWindowsService() error {
	return svc.Run(serviceName, &service{})
}

func (s *service) Execute(args []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}

	// The Execute args from the SCM dispatcher only contain runtime start parameters
	// (typically just [serviceName]). The actual configured arguments are baked into
	// os.Args via the service ImagePath. Parse from os.Args to get the real values,
	// falling back to defaults if not present.
	dataPath, logPath, logLevel := parseServiceArgs(os.Args[1:])

	// Run the daemon as a child process so we can clean up network state if it crashes,
	// regardless of whether the SCM is configured to restart the service.
	childArgs := []string{"run", "--data-path", dataPath, "--log-path", logPath, "--log-level", logLevel}
	child, err := spawnChild(childArgs, dataPath, logPath, logLevel)
	if err != nil {
		slog.Error("Failed to start daemon", "error", err)
		return true, 1
	}

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	child.logger.Info("Running as Windows service")

	for {
		select {
		case err := <-child.Done():
			if err != nil {
				child.HandleCrash(err)
			}
			return true, 1
		case change := <-r:
			switch change.Cmd {
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				child.logger.Info("Service stop requested")
				child.RequestShutdown()
				child.WaitOrKill(15 * time.Second)
				return false, windows.NO_ERROR
			case svc.Interrogate:
				status <- change.CurrentStatus
			case svc.SessionChange:
				status <- change.CurrentStatus
			}
		}
	}
}

func parseServiceArgs(args []string) (dataPath, logPath, logLevel string) {
	dataPath = os.ExpandEnv(defaultDataPath)
	logPath = os.ExpandEnv(defaultLogPath)
	logLevel = "info"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--data-path":
			if i+1 < len(args) {
				dataPath = os.ExpandEnv(args[i+1])
				i++
			}
		case "--log-path":
			if i+1 < len(args) {
				logPath = os.ExpandEnv(args[i+1])
				i++
			}
		case "--log-level":
			if i+1 < len(args) {
				logLevel = args[i+1]
				i++
			}
		}
	}
	return
}
