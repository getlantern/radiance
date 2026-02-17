//go:build !windows

package main

import (
	"flag"
	"log"
)

const (
	defaultDataPath = "$HOME/.lantern"
	defaultLogPath  = "$HOME/.lantern"
)

var (
	dataPath = flag.String("data-path", "$HOME/.lantern", "Path to store data")
	logPath  = flag.String("log-path", "$HOME/.lantern", "Path to store logs")
	logLevel = flag.String("log-level", "info", "Logging level (trace, debug, info, warn, error)")
)

const isWindowsService = false

func startWindowsService() error { panic("Only supported on Windows") }

func run() {
	flag.Parse()
	if err := runDaemon(*dataPath, *logPath, *logLevel); err != nil {
		log.Fatalf("Error running lanternd: %v\n", err)
	}
}
