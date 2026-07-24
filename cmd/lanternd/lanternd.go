package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alexflint/go-arg"

	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common"
	commonenv "github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/ipc"
	rlog "github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/vpn"
)

type runCmd struct {
	DataPath    string            `arg:"--data-path" help:"path to store data"`
	LogPath     string            `arg:"--log-path" help:"path to store logs"`
	LogLevel    string            `arg:"--log-level" default:"info" help:"logging level (trace, debug, info, warn, error)"`
	Environment daemonEnvironment `arg:"--environment" default:"prod" help:"backend environment (prod or staging)"`
}

type installCmd struct {
	DataPath    string            `arg:"--data-path" help:"path to store data"`
	LogPath     string            `arg:"--log-path" help:"path to store logs"`
	LogLevel    string            `arg:"--log-level" default:"info" help:"logging level (trace, debug, info, warn, error)"`
	Environment daemonEnvironment `arg:"--environment" default:"prod" help:"backend environment (prod or staging)"`
}

type daemonEnvironment string

const (
	daemonEnvironmentProd    daemonEnvironment = "prod"
	daemonEnvironmentStaging daemonEnvironment = "staging"
)

// UnmarshalText validates the environment while go-arg parses the command line.
func (e *daemonEnvironment) UnmarshalText(text []byte) error {
	parsed, err := parseDaemonEnvironment(string(text))
	if err != nil {
		return err
	}
	*e = parsed
	return nil
}

// parseDaemonEnvironment keeps lanternd from silently accepting an unknown environment.
func parseDaemonEnvironment(value string) (daemonEnvironment, error) {
	environment := daemonEnvironment(value)
	switch environment {
	case daemonEnvironmentProd, daemonEnvironmentStaging:
		return environment, nil
	default:
		return "", fmt.Errorf("unsupported environment %q (must be prod or staging)", value)
	}
}

type serviceRunConfig struct {
	dataPath    string
	logPath     string
	logLevel    string
	environment daemonEnvironment
}

// args builds the command line used to start a daemon with this service configuration.
func (c serviceRunConfig) args() []string {
	return []string{
		"run",
		"--data-path", c.dataPath,
		"--log-path", c.logPath,
		"--log-level", c.logLevel,
		"--environment", string(c.environment),
	}
}

// parseServiceRunArgs recovers the persisted daemon configuration from a service command line.
func parseServiceRunArgs(args []string) (serviceRunConfig, error) {
	config := serviceRunConfig{
		dataPath:    internal.DefaultDataPath(),
		logPath:     internal.DefaultLogPath(),
		logLevel:    "info",
		environment: daemonEnvironmentProd,
	}
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			switch args[i] {
			case "--data-path", "--log-path", "--log-level", "--environment":
				return serviceRunConfig{}, fmt.Errorf("missing value for %s", args[i])
			}
			continue
		}
		switch args[i] {
		case "--data-path":
			config.dataPath = os.ExpandEnv(args[i+1])
			i++
		case "--log-path":
			config.logPath = os.ExpandEnv(args[i+1])
			i++
		case "--log-level":
			config.logLevel = args[i+1]
			i++
		case "--environment":
			environment, err := parseDaemonEnvironment(args[i+1])
			if err != nil {
				return serviceRunConfig{}, err
			}
			config.environment = environment
			i++
		}
	}
	return config, nil
}

type uninstallCmd struct{}

type versionCmd struct{}

type daemonArgs struct {
	Run       *runCmd       `arg:"subcommand:run" help:"run the daemon"`
	Install   *installCmd   `arg:"subcommand:install" help:"install as system service"`
	Uninstall *uninstallCmd `arg:"subcommand:uninstall" help:"uninstall system service"`
	Version   *versionCmd   `arg:"subcommand:version" help:"print version"`
}

func (daemonArgs) Description() string {
	return "lanternd — Lantern VPN daemon"
}

func init() {
	log.SetFlags(log.Lshortfile | log.LstdFlags)
}

func main() {
	if maybePlatformService() {
		return
	}

	var a daemonArgs
	p := arg.MustParse(&a)
	if p.Subcommand() == nil {
		p.WriteHelp(os.Stdout)
		os.Exit(1)
	}

	defaultDataPath := internal.DefaultDataPath()
	defaultLogPath := internal.DefaultLogPath()
	var err error
	switch {
	case a.Run != nil:
		dataPath := os.ExpandEnv(withDefault(a.Run.DataPath, defaultDataPath))
		logPath := os.ExpandEnv(withDefault(a.Run.LogPath, defaultLogPath))
		if os.Getenv("_LANTERND_CHILD") != "1" {
			err = babysit(os.Args[1:], dataPath, logPath, a.Run.LogLevel)
			break
		}
		ctx, cancel := context.WithCancel(context.Background())
		// Shut down on stdin closure (babysit parent signals us) or OS signal.
		go func() {
			io.Copy(io.Discard, os.Stdin)
			cancel()
		}()
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			cancel()
			// Restore default signal behavior so a second signal terminates immediately.
			signal.Reset(syscall.SIGINT, syscall.SIGTERM)
		}()
		err = runDaemon(ctx, dataPath, logPath, a.Run.LogLevel, a.Run.Environment)
	case a.Install != nil:
		err = install(
			os.ExpandEnv(withDefault(a.Install.DataPath, defaultDataPath)),
			os.ExpandEnv(withDefault(a.Install.LogPath, defaultLogPath)),
			a.Install.LogLevel,
			a.Install.Environment,
		)
	case a.Uninstall != nil:
		err = uninstall()
	case a.Version != nil:
		fmt.Println(common.Version)
	}
	if err != nil {
		log.Fatalf("Error: %v\n", err)
	}
}

func withDefault(val, def string) string {
	if val == "" {
		return def
	}
	return val
}

// copyBin copies the current executable to binPath, creating parent directories
// as needed. It returns the destination path.
func copyBin() (string, error) {
	src, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to get executable path: %w", err)
	}
	src, err = filepath.EvalSymlinks(src)
	if err != nil {
		return "", fmt.Errorf("failed to resolve executable path: %w", err)
	}

	dst := binPath
	if src == dst {
		return dst, nil
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("failed to create directory for %s: %w", dst, err)
	}

	sf, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("failed to open source binary: %w", err)
	}
	defer sf.Close()

	df, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", fmt.Errorf("failed to create %s: %w", dst, err)
	}
	defer df.Close()

	if _, err := io.Copy(df, sf); err != nil {
		return "", fmt.Errorf("failed to copy binary to %s: %w", dst, err)
	}

	slog.Info("Copied binary", "src", src, "dst", dst)
	return dst, nil
}

// childProcess manages a daemon child process. The parent spawns the child, drains its output,
// and can signal graceful shutdown by closing its stdin pipe. If the child crashes, the parent
// cleans up stale VPN network state immediately.
type childProcess struct {
	cmd      *exec.Cmd
	stdin    io.Closer
	done     chan error
	dataPath string
	logger   *slog.Logger
}

// spawnChild creates and starts a daemon child process with piped I/O. The child's stdout and
// stderr are merged and drained through the provided logger (or os.Stdout as fallback).
func spawnChild(args []string, dataPath, logPath, logLevel string) (*childProcess, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("failed to get executable path: %w", err)
	}

	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "_LANTERND_CHILD=1")
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout // merge stderr into the same pipe

	logger := rlog.NewLogger(rlog.Config{
		LogPath:          filepath.Join(logPath, internal.LogFileName),
		Level:            logLevel,
		Prod:             true,
		DisablePublisher: true,
	})

	go func() {
		defer stdoutPipe.Close()
		var w io.Writer = os.Stdout
		// TODO: the child process outputs to both stdout and the file logger, so we end up with
		// 	duplicate log lines. we'll come back to this later and fix it, but for now just
		// 	write to stdout to avoid the duplication.
		// if h, ok := logger.Handler().(*rlog.Handler); ok {
		// 	w = h.Writer()
		// }
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			if s := scanner.Text(); s != "" {
				w.Write([]byte(s + "\n"))
			}
		}
		if err := scanner.Err(); err != nil {
			logger.Error("Error reading child process output", "error", err)
		}
	}()

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start daemon process: %w", err)
	}
	logger.Info("Started daemon process", "pid", cmd.Process.Pid)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	return &childProcess{
		cmd:      cmd,
		stdin:    stdinPipe,
		done:     done,
		dataPath: dataPath,
		logger:   logger,
	}, nil
}

// RequestShutdown signals the child to shut down gracefully by closing its stdin pipe.
func (c *childProcess) RequestShutdown() {
	c.logger.Info("Requesting child process shutdown")
	c.stdin.Close()
}

// Done returns a channel that receives the child's exit error (nil on clean exit).
func (c *childProcess) Done() <-chan error {
	return c.done
}

// WaitOrKill waits for the child to exit, killing it if it doesn't exit within the timeout.
func (c *childProcess) WaitOrKill(timeout time.Duration) error {
	select {
	case err := <-c.done:
		return err
	case <-time.After(timeout):
		c.logger.Warn("Child did not exit in time, killing")
		c.cmd.Process.Kill()
		return <-c.done
	}
}

// HandleCrash cleans up stale VPN network state left by a crashed child.
func (c *childProcess) HandleCrash(err error) {
	c.logger.Warn("Daemon process exited unexpectedly, cleaning up network state", "error", err)
	vpn.AttemptFixNetState()
}

// babysit runs the daemon as a child process and monitors it. If the child exits unexpectedly
// (crash, panic, etc.), the parent immediately cleans up any stale VPN network state and
// automatically restarts the child process with exponential backoff.
//
// Graceful shutdown is signaled by closing the child's stdin pipe — this works cross-platform,
// including inside a Windows service where there is no console for signal delivery.
func babysit(args []string, dataPath, logPath, logLevel string) error {
	// On termination signal, request graceful shutdown of the current child.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	stopping := false

	const resetAfter = 2 * time.Minute // reset backoff if child ran longer than this
	bo := common.NewBackoff(60 * time.Second)

	for {
		child, err := spawnChild(args, dataPath, logPath, logLevel)
		if err != nil {
			if stopping {
				return nil
			}
			return err
		}
		child.logger.Info("Monitoring daemon process")
		startedAt := time.Now()

		// Wait for either a termination signal or child exit.
		select {
		case sig := <-sigCh:
			stopping = true
			child.logger.Info("Received signal, shutting down child", "signal", sig)
			child.RequestShutdown()
			err = child.WaitOrKill(15 * time.Second)
		case err = <-child.Done():
		}

		if stopping {
			signal.Stop(sigCh)
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				os.Exit(exitErr.ExitCode())
			}
			return err
		}

		// Unexpected exit — clean up and restart.
		if err != nil {
			child.HandleCrash(err)
		}

		// Reset backoff if the child ran for a while (i.e. it wasn't a fast crash loop).
		if time.Since(startedAt) > resetAfter {
			bo.Reset()
		}

		child.logger.Info("Restarting child process")
		bo.Wait(context.Background())
	}
}

// daemonBackendOptions passes the selected daemon environment through to the backend.
func daemonBackendOptions(dataPath, logPath, logLevel string, environment daemonEnvironment) backend.Options {
	return backend.Options{
		DataDir:  dataPath,
		LogDir:   logPath,
		LogLevel: logLevel,
		EnvOverrides: map[string]string{
			commonenv.ENV.String(): string(environment),
		},
	}
}

// daemonBackendURLs returns the endpoints that the backend will use for the selected environment.
func daemonBackendURLs(environment daemonEnvironment) (authURL, proServerURL string) {
	if environment == daemonEnvironmentStaging {
		return common.StageBaseURL, common.StageProServerURL
	}
	return common.BaseURL, common.ProServerURL
}

func runDaemon(ctx context.Context, dataPath, logPath, logLevel string, environment daemonEnvironment) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	authURL, proServerURL := daemonBackendURLs(environment)
	slog.Info("Starting lanternd", "version", common.Version, "dataPath", dataPath, "environment", environment, "authURL", authURL, "proServerURL", proServerURL)
	be, err := backend.NewLocalBackend(ctx, daemonBackendOptions(dataPath, logPath, logLevel, environment))
	if err != nil {
		return fmt.Errorf("failed to create backend: %w", err)
	}
	user, err := be.UserData()
	if err != nil {
		return fmt.Errorf("failed to get current data: %w", err)
	}
	if user == nil {
		if _, err := be.NewUser(ctx); err != nil {
			return fmt.Errorf("failed to create new user: %w", err)
		}
	}

	be.Start()
	server := ipc.NewServer(be, !common.IsMobile())
	if err := server.Start(); err != nil {
		return fmt.Errorf("failed to start IPC server: %w", err)
	}

	// Wait for context cancellation to gracefully shut down.
	<-ctx.Done()

	slog.Info("Shutting down...")

	time.AfterFunc(15*time.Second, func() {
		slog.Error("Failed to shut down in time, forcing exit")
		os.Exit(1)
	})

	be.Close()
	if err := server.Close(); err != nil {
		slog.Error("Error closing IPC server", "error", err)
	}
	slog.Info("Shutdown complete")
	return nil
}
