package main

import (
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
	"sync/atomic"
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

// UnmarshalText rejects unsupported daemon environments during CLI decoding.
func (e *daemonEnvironment) UnmarshalText(text []byte) error {
	parsed, err := parseDaemonEnvironment(string(text))
	if err != nil {
		return err
	}
	*e = parsed
	return nil
}

func parseDaemonEnvironment(value string) (daemonEnvironment, error) {
	environment := daemonEnvironment(value)
	switch environment {
	case daemonEnvironmentProd, daemonEnvironmentStaging:
		return environment, nil
	default:
		return "", fmt.Errorf("unsupported environment %q (must be prod or staging)", value)
	}
}

// gracefulShutdownTimeout is the maximum time the supervisor waits for a child
// to exit after requesting a graceful shutdown.
const gracefulShutdownTimeout = 15 * time.Second

// childForceExitTimeout gives the child time to log a forced-exit reason before
// the supervisor's shutdown deadline expires.
const childForceExitTimeout = gracefulShutdownTimeout - 3*time.Second

// childWaitDelay bounds how long Wait may block on inherited stdout/stderr
// pipes after the child exits.
const childWaitDelay = 5 * time.Second

// childEnvMarker marks a supervised child process so main runs the daemon
// directly instead of starting another supervisor.
const childEnvMarker = "_LANTERND_CHILD"

type serviceRunConfig struct {
	dataPath    string
	logPath     string
	logLevel    string
	environment daemonEnvironment
}

func (c runCmd) serviceConfig() serviceRunConfig {
	return serviceRunConfig{
		dataPath:    os.ExpandEnv(withDefault(c.DataPath, internal.DefaultDataPath())),
		logPath:     os.ExpandEnv(withDefault(c.LogPath, internal.DefaultLogPath())),
		logLevel:    c.LogLevel,
		environment: c.Environment,
	}
}

func (c serviceRunConfig) args() []string {
	return []string{
		"run",
		"--data-path", c.dataPath,
		"--log-path", c.logPath,
		"--log-level", c.logLevel,
		"--environment", string(c.environment),
	}
}

func parseServiceRunArgs(args []string) (serviceRunConfig, error) {
	var parsed daemonArgs
	parser, err := arg.NewParser(arg.Config{}, &parsed)
	if err != nil {
		return serviceRunConfig{}, fmt.Errorf("create service argument parser: %w", err)
	}
	if err := parser.Parse(args); err != nil {
		return serviceRunConfig{}, fmt.Errorf("parse service arguments: %w", err)
	}
	if parsed.Run == nil {
		return serviceRunConfig{}, errors.New("service command must run the daemon")
	}
	return parsed.Run.serviceConfig(), nil
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
		config := a.Run.serviceConfig()
		if os.Getenv(childEnvMarker) != "1" {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			logger := newDaemonLogger(config.logPath, config.logLevel)
			err = babysit(ctx, os.Args[1:], logger, nil)
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				os.Exit(exitErr.ExitCode())
			}
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
		err = runDaemon(ctx, config.dataPath, config.logPath, config.logLevel, config.environment)
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

// newDaemonLogger returns the supervisor logger for the daemon log file.
func newDaemonLogger(logPath, logLevel string) *slog.Logger {
	// The child creates this directory in common.Init, but the supervisor logs — and starts
	// draining the child — before that. A failure is not fatal: lumberjack retries the MkdirAll on
	// its first write, and refusing to run the daemon over its log directory would be worse.
	if err := os.MkdirAll(logPath, 0o755); err != nil {
		slog.Warn("Failed to create log directory", "error", err, "path", logPath)
	}
	return rlog.NewLogger(rlog.Config{
		LogPath:          filepath.Join(logPath, internal.LogFileName),
		Level:            logLevel,
		Prod:             true,
		DisablePublisher: true,
	})
}

type childProcess struct {
	cmd    *exec.Cmd
	stdin  io.Closer
	done   chan error
	logger *slog.Logger
}

func childEnv() []string {
	return append(os.Environ(), childEnvMarker+"=1", commonenv.LogToStdout.String()+"=true")
}

// spawnChild starts the daemon as a supervised child process.
// Child stdout and stderr are written directly to the supervisor's log writer.
func spawnChild(args []string, logger *slog.Logger) (*childProcess, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("failed to get executable path: %w", err)
	}

	cmd := exec.Command(exe, args...)
	cmd.Env = childEnv()
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	// The child writes preformatted log lines to stdout. Write them directly to
	// the supervisor's output sink to avoid double formatting.
	var w io.Writer = os.Stdout
	if h, ok := logger.Handler().(*rlog.Handler); ok {
		w = h.Writer()
	}
	// Same writer value for both streams so output stays serialized through one sink.
	out := &bestEffortWriter{w: w}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = childWaitDelay

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start daemon process: %w", err)
	}
	logger.Info("Started daemon process", "pid", cmd.Process.Pid)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	return &childProcess{
		cmd:    cmd,
		stdin:  stdinPipe,
		done:   done,
		logger: logger,
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

// childExitError treats ErrWaitDelay as non-fatal after process exit.
func childExitError(err error) error {
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return err
}

// bestEffortWriter drops output rather than reporting a write error. [exec.Cmd] closes the child's
// output pipe when its copy fails, and a Go program whose stdout or stderr pipe breaks exits on
// SIGPIPE, so surfacing a log-sink failure here would crash-loop the daemon over unwritable logs.
type bestEffortWriter struct {
	w      io.Writer
	failed atomic.Bool
}

func (b *bestEffortWriter) Write(p []byte) (int, error) {
	if _, err := b.w.Write(p); err != nil && !b.failed.Swap(true) {
		// Reported on stderr because the sink that just failed cannot report its own failure.
		fmt.Fprintf(os.Stderr, "lanternd: dropping daemon output, log writer failed: %v\n", err)
	}
	return len(p), nil
}

// HandleCrash cleans up stale VPN network state left by a crashed child.
func (c *childProcess) HandleCrash(err error) {
	c.logger.Warn("Daemon process exited unexpectedly, cleaning up network state", "error", err)
	vpn.AttemptFixNetState()
}

// babysit runs the daemon as a child process until ctx is canceled.
// Unexpected exits trigger cleanup and restart with backoff.
//
// Graceful shutdown is requested by closing the child's stdin, which works even
// in service environments without console signal delivery.
//
// ready, if non-nil, is called after the first child starts.
//
// The returned error is nil, a spawn failure, or the last child exit error.
func babysit(ctx context.Context, args []string, logger *slog.Logger, ready func()) error {
	const resetAfter = 2 * time.Minute // reset backoff if child ran longer than this
	bo := common.NewBackoff(60 * time.Second)

	for ctx.Err() == nil {
		child, err := spawnChild(args, logger)
		if err != nil {
			return err
		}
		if ready != nil {
			ready()
			ready = nil
		}
		logger.Info("Monitoring daemon process")
		startedAt := time.Now()

		select {
		case <-ctx.Done():
			logger.Info("Shutdown requested, stopping daemon process")
			child.RequestShutdown()
			return childExitError(child.WaitOrKill(gracefulShutdownTimeout))
		case err = <-child.Done():
			err = childExitError(err)
		}

		if err != nil && ctx.Err() == nil {
			child.HandleCrash(err)
		}

		if time.Since(startedAt) > resetAfter {
			bo.Reset()
		}

		logger.Info("Restarting child process")
		bo.Wait(ctx)
	}
	return nil
}

// daemonBackendOptions overrides staging only, preserving any externally
// configured development environment in production mode.
func daemonBackendOptions(dataPath, logPath, logLevel string, environment daemonEnvironment) backend.Options {
	options := backend.Options{
		DataDir:  dataPath,
		LogDir:   logPath,
		LogLevel: logLevel,
	}
	if environment == daemonEnvironmentStaging {
		options.EnvOverrides = map[string]string{
			commonenv.ENV.String(): string(environment),
		}
	}
	return options
}

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

	<-ctx.Done()

	slog.Info("Shutting down...")

	time.AfterFunc(childForceExitTimeout, func() {
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
