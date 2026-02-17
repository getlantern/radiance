package main

import (
	"flag"
	"log"
	"log/slog"
	"os"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

const (
	defaultDataPath = "$PROGRAMDATA\\lantern"
	defaultLogPath  = "$PROGRAMDATA\\lantern"
)

var isWindowsService bool

func init() {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		log.Fatalf("Failed to determine if running as Windows service: %v\n", err)
	}
	isWindowsService = isSvc
}

func run() {
	args := os.Args[1:]
	if len(args) == 0 {
		log.Fatalf("No command provided. Usage: %s [run|install|uninstall]\n", os.Args[0])
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		dataPath := fs.String("data-path", defaultDataPath, "Path to store data")
		logPath := fs.String("log-path", defaultLogPath, "Path to store logs")
		logLevel := fs.String("log-level", "info", "Logging level (trace, debug, info, warn, error)")
		fs.Parse(args)
		if err := runDaemon(*dataPath, *logPath, *logLevel); err != nil {
			log.Fatalf("Error running lanternd: %v\n", err)
		}
	case "install":
		fs := flag.NewFlagSet("install", flag.ExitOnError)
		dataPath := fs.String("data-path", defaultDataPath, "Path to store data")
		logPath := fs.String("log-path", defaultLogPath, "Path to store logs")
		logLevel := fs.String("log-level", "info", "Logging level (trace, debug, info, warn, error)")
		fs.Parse(args)
		if err := install(*dataPath, *logPath, *logLevel); err != nil {
			log.Fatalf("Error running lanternd: %v\n", err)
		}
	case "uninstall":
		if err := uninstall(); err != nil {
			log.Fatalf("Error uninstalling lanternd: %v\n", err)
		}
	default:
		log.Fatalf("Unknown command: %q. Usage: %s [run|install|uninstall]\n", args[0], os.Args[0])
	}
}

type service struct{}

func startWindowsService() error {
	return svc.Run(serviceName, &service{})
}

func (s *service) Execute(args []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}

	svcAccepts := svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.Running, Accepts: svcAccepts}
	slog.Info("Running as Windows service")
	for cmd := range r {
		switch cmd.Cmd {
		case svc.Stop, svc.Shutdown:
			status <- svc.Status{State: svc.StopPending}
			slog.Info("Service stop pending")
			return false, windows.NO_ERROR
		case svc.Interrogate:
			slog.Info("Service interrogation")
			status <- cmd.CurrentStatus
		case svc.SessionChange:
			slog.Info("Service session change notification")
			status <- cmd.CurrentStatus
		}
	}
	return false, windows.NO_ERROR
}
