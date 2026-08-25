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

	"github.com/getlantern/radiance/common"
)

// serviceStopWait covers the supervisor's full shutdown path so the service
// stop handler does not time out while babysit is still finishing.
const serviceStopWait = gracefulShutdownTimeout + childWaitDelay + 5*time.Second

// serviceRestartDelays defines the SCM restart policy for failures of the
// supervising service process itself.
var serviceRestartDelays = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
	32 * time.Second,
	64 * time.Second,
}

// serviceFailureResetPeriod returns how long the service must run without
// failing before the SCM resets its failure count.
func serviceFailureResetPeriod() uint32 {
	var span time.Duration
	for _, delay := range serviceRestartDelays {
		span += delay
	}
	return uint32((2 * span).Seconds())
}

const (
	serviceName = "LanternSvc"
	binPath     = "C:\\Program Files\\Lantern\\" + serviceName + ".exe"
)

var isWindowsService bool

func init() {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		log.Fatalf("Failed to determine if running as Windows service: %v\n", err)
	}
	isWindowsService = isSvc
}

func install(dataPath, logPath, logLevel string, environment daemonEnvironment) error {
	dataPath = os.ExpandEnv(dataPath)
	logPath = os.ExpandEnv(logPath)

	slog.Info("Installing Windows service..", "version", common.Version)

	// Remove any existing service so we can recreate it cleanly.
	// Errors are expected on first install when no service exists yet.
	if err := uninstall(); err != nil {
		slog.Debug("No existing service to remove (expected on first install)", "error", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to Windows service manager: %w", err)
	}
	defer m.Disconnect()

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

	args := (serviceRunConfig{
		dataPath:    dataPath,
		logPath:     logPath,
		logLevel:    logLevel,
		environment: environment,
	}).args()

	slog.Info("Creating Windows service", "exe", exe, "args", args)
	svcHandle, err := m.CreateService(serviceName, exe, config, args...)
	if err != nil {
		return fmt.Errorf("failed to create %q service: %w", serviceName, err)
	}
	defer svcHandle.Close()

	actions := make([]mgr.RecoveryAction, 0, len(serviceRestartDelays)+1)
	for _, delay := range serviceRestartDelays {
		actions = append(actions, mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: delay})
	}
	// The SCM repeats the final recovery action indefinitely, so append NoAction to
	// make the retry ladder terminate.
	actions = append(actions, mgr.RecoveryAction{Type: mgr.NoAction})

	err = svcHandle.SetRecoveryActions(actions, serviceFailureResetPeriod())
	if err != nil {
		return fmt.Errorf("failed to set service recovery actions: %w", err)
	}
	// Execute always reports SERVICE_STOPPED on return, so enable recovery actions
	// for non-crash failures as well.
	if err := svcHandle.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("failed to enable recovery actions on non-crash failures: %w", err)
	}
	if err := svcHandle.Start(); err != nil {
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

	svcHandle, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("failed to open %q service: %w", serviceName, err)
	}

	status, err := svcHandle.Query()
	if err != nil {
		svcHandle.Close()
		return fmt.Errorf("failed to query service state: %w", err)
	}
	if status.State != svc.Stopped {
		// Continue with deletion even if stop fails; the service will be removed once
		// its process exits.
		if _, err := svcHandle.Control(svc.Stop); err != nil {
			slog.Warn("Failed to stop service before deleting", "error", err)
		}
	}
	err = svcHandle.Delete()
	svcHandle.Close()
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
			if svcHandle, err = m.OpenService(serviceName); err != nil {
				slog.Info("Windows service uninstalled successfully")
				if err := os.Remove(binPath); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("failed to remove binary: %w", err)
				}
				return nil
			}
			svcHandle.Close()
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
	config, err := parseServiceRunArgs(os.Args[1:])
	if err != nil {
		slog.Error("Failed to parse service arguments", "error", err)
		return true, 1
	}
	logger := newDaemonLogger(config.logPath, config.logLevel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	supervisor := make(chan error, 1)
	running := make(chan struct{})
	go func() {
		supervisor <- babysit(ctx, config.args(), logger, func() { close(running) })
	}()

	// Delay reporting Running until the first child starts, so startup failures are
	// reported as service start failures.
	select {
	case <-running:
	case err := <-supervisor:
		logger.Error("Daemon failed to start", "error", err)
		return true, 1
	}

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	logger.Info("Running as Windows service")

	for {
		select {
		case err := <-supervisor:
			logger.Error("Daemon supervisor exited, stopping service", "error", err)
			return true, 1
		case change := <-r:
			switch change.Cmd {
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{
					State:      svc.StopPending,
					CheckPoint: 1,
					WaitHint:   uint32(serviceStopWait.Milliseconds()),
				}
				logger.Info("Service stop requested")
				cancel()
				select {
				case err := <-supervisor:
					if err != nil {
						logger.Warn("Daemon did not stop cleanly", "error", err)
					}
				case <-time.After(serviceStopWait):
					logger.Warn("Daemon supervisor did not exit in time", "wait", serviceStopWait)
				}
				// Report a clean stop even on timeout to avoid SCM-triggered restart racing a
				// child process that may still be exiting.
				return false, windows.NO_ERROR
			case svc.Interrogate:
				status <- change.CurrentStatus
			case svc.SessionChange:
				status <- change.CurrentStatus
			}
		}
	}
}
