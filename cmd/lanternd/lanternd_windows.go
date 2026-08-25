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

const (
	serviceName = "LanternSvc"
	binPath     = "C:\\Program Files\\Lantern\\" + serviceName + ".exe"

	// windowsServiceChildShutdownTimeout bounds the child's graceful shutdown period.
	windowsServiceChildShutdownTimeout = 15 * time.Second
	// windowsServiceStopWaitHint includes headroom for forced termination after that period.
	windowsServiceStopWaitHint = 20 * time.Second
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
	service, err := m.CreateService(serviceName, exe, config, args...)
	if err != nil {
		return fmt.Errorf("failed to create %q service: %w", serviceName, err)
	}
	defer service.Close()

	if err := configureWindowsServiceRecovery(service); err != nil {
		return err
	}
	if err := service.Start(); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}

	slog.Info("Windows service installed successfully")
	return nil
}

type windowsServiceRecoveryConfigurer interface {
	SetRecoveryActions([]mgr.RecoveryAction, uint32) error
	SetRecoveryActionsOnNonCrashFailures(bool) error
}

func configureWindowsServiceRecovery(service windowsServiceRecoveryConfigurer) error {
	if err := service.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 1 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 2 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 4 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 8 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 16 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 32 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 64 * time.Second},
	}, 60); err != nil {
		return fmt.Errorf("failed to set service recovery actions: %w", err)
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("failed to enable recovery actions for non-crash failures: %w", err)
	}
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
				if err := os.Remove(binPath); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("failed to remove binary: %w", err)
				}
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

type windowsServiceChild interface {
	Done() <-chan error
	RequestShutdown()
	WaitOrKill(time.Duration) error
	HandleCrash(error)
	info(string, ...any)
}

type windowsServiceChildProcess struct {
	*childProcess
}

func (c *windowsServiceChildProcess) info(message string, args ...any) {
	c.logger.Info(message, args...)
}

type windowsServiceBackoff interface {
	Wait(context.Context)
	Reset()
}

type service struct {
	spawnChild func([]string, string, string, string) (windowsServiceChild, error)
	newBackoff func() windowsServiceBackoff
}

// newWindowsService returns a production service handler. Its injected process and backoff
// functions keep supervision tests independent of OS processes and wall-clock delays.
func newWindowsService() *service {
	return &service{
		spawnChild: func(args []string, dataPath, logPath, logLevel string) (windowsServiceChild, error) {
			child, err := spawnChild(args, dataPath, logPath, logLevel)
			if err != nil {
				return nil, err
			}
			return &windowsServiceChildProcess{childProcess: child}, nil
		},
		newBackoff: func() windowsServiceBackoff {
			return common.NewBackoff(daemonRestartBackoffMax)
		},
	}
}

func startWindowsService() error {
	return svc.Run(serviceName, newWindowsService())
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
	return s.run(config, r, status)
}

// run supervises the daemon child so crash cleanup and restart do not depend on SCM recovery.
func (s *service) run(config serviceRunConfig, r <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	childArgs := config.args()
	child, err := s.spawnChild(childArgs, config.dataPath, config.logPath, config.logLevel)
	if err != nil {
		slog.Error("Failed to start daemon", "error", err)
		return true, 1
	}

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	child.info("Running as Windows service")

	backoff := s.newBackoff()
	startedAt := time.Now()
	childDone := child.Done()
	var restartReady <-chan struct{}
	serviceContext, cancelService := context.WithCancel(context.Background())
	defer cancelService()

	for {
		select {
		case err := <-childDone:
			if err != nil {
				child.HandleCrash(err)
			}
			if time.Since(startedAt) > daemonRestartBackoffResetAfter {
				backoff.Reset()
			}
			child.info("Restarting daemon process")
			child = nil
			childDone = nil

			restartDone := make(chan struct{})
			restartReady = restartDone
			go func() {
				backoff.Wait(serviceContext)
				close(restartDone)
			}()
		case <-restartReady:
			restartReady = nil

			child, err = s.spawnChild(childArgs, config.dataPath, config.logPath, config.logLevel)
			if err != nil {
				slog.Error("Failed to restart daemon", "error", err)
				return true, 1
			}
			startedAt = time.Now()
			childDone = child.Done()
			child.info("Running as Windows service")
		case change := <-r:
			switch change.Cmd {
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{
					State:      svc.StopPending,
					CheckPoint: 1,
					WaitHint:   uint32(windowsServiceStopWaitHint / time.Millisecond),
				}
				cancelService()
				if restartReady != nil {
					<-restartReady
				}
				if child != nil {
					child.info("Service stop requested")
					child.RequestShutdown()
					if err := child.WaitOrKill(windowsServiceChildShutdownTimeout); err != nil {
						slog.Warn("Daemon process did not stop cleanly", "error", err)
					}
				}
				return false, windows.NO_ERROR
			case svc.Interrogate:
				status <- change.CurrentStatus
			case svc.SessionChange:
				status <- change.CurrentStatus
			}
		}
	}
}
