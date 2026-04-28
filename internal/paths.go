package internal

import (
	"os"
	"path/filepath"
	"runtime"
)

const (
	DebugBoxOptionsFileName = "debug-box-options.json"
	ConfigFileName          = "config.json"
	ServersFileName         = "servers.json"
	SplitTunnelFileName     = "split-tunnel.json"
	LogFileName             = "lantern.log"
	CrashLogFileName        = "lantern-crash.log"
)

func DefaultDataPath() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("ProgramData"), "Lantern")
	case "darwin":
		return "/Library/Application Support/Lantern"
	case "linux":
		return "/var/lib/lantern"
	default:
		return ""
	}
}

func DefaultLogPath() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("ProgramData"), "Lantern")
	case "darwin":
		return "/Library/Logs/Lantern"
	case "linux":
		return "/var/log/lantern"
	default:
		return ""
	}
}
