package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

type fakeWindowsServiceChild struct {
	done      chan error
	crashes   chan error
	shutdowns chan struct{}
	waits     chan time.Duration
	waitErr   error
}

func newFakeWindowsServiceChild() *fakeWindowsServiceChild {
	return &fakeWindowsServiceChild{
		done:      make(chan error, 1),
		crashes:   make(chan error, 1),
		shutdowns: make(chan struct{}, 1),
		waits:     make(chan time.Duration, 1),
	}
}

func (c *fakeWindowsServiceChild) Done() <-chan error {
	return c.done
}

func (c *fakeWindowsServiceChild) RequestShutdown() {
	c.shutdowns <- struct{}{}
}

func (c *fakeWindowsServiceChild) WaitOrKill(timeout time.Duration) error {
	c.waits <- timeout
	return c.waitErr
}

func (c *fakeWindowsServiceChild) HandleCrash(err error) {
	c.crashes <- err
}

func (c *fakeWindowsServiceChild) info(string, ...any) {}

type immediateWindowsServiceBackoff struct{}

func (immediateWindowsServiceBackoff) Wait(context.Context) {}
func (immediateWindowsServiceBackoff) Reset()               {}

type blockingWindowsServiceBackoff struct {
	started chan struct{}
	stopped chan struct{}
	resets  chan struct{}
}

func (b *blockingWindowsServiceBackoff) Wait(ctx context.Context) {
	b.started <- struct{}{}
	<-ctx.Done()
	b.stopped <- struct{}{}
}

func (b *blockingWindowsServiceBackoff) Reset() {
	b.resets <- struct{}{}
}

type windowsServiceResult struct {
	serviceSpecificExitCode bool
	exitCode                uint32
}

func TestWindowsServiceRestartsExitedChildren(t *testing.T) {
	children := []*fakeWindowsServiceChild{
		newFakeWindowsServiceChild(),
		newFakeWindowsServiceChild(),
		newFakeWindowsServiceChild(),
	}
	spawned := make(chan *fakeWindowsServiceChild, len(children))
	spawnIndex := 0
	windowsService := &service{
		spawnChild: func([]string, string, string, string) (windowsServiceChild, error) {
			child := children[spawnIndex]
			spawnIndex++
			spawned <- child
			return child, nil
		},
		newBackoff: func() windowsServiceBackoff { return immediateWindowsServiceBackoff{} },
	}

	requests := make(chan svc.ChangeRequest, 1)
	statuses := make(chan svc.Status, 2)
	result := executeWindowsService(windowsService, requests, statuses)

	require.Same(t, children[0], receive(t, spawned))
	require.Equal(t, svc.Running, receive(t, statuses).State)

	crashErr := errors.New("daemon crashed")
	children[0].done <- crashErr
	require.ErrorIs(t, receive(t, children[0].crashes), crashErr)
	require.Same(t, children[1], receive(t, spawned))

	children[1].done <- nil
	require.Same(t, children[2], receive(t, spawned))
	select {
	case err := <-children[1].crashes:
		t.Fatalf("clean child exit handled as crash: %v", err)
	default:
	}

	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	require.Equal(t, svc.StopPending, receive(t, statuses).State)
	receive(t, children[2].shutdowns)
	require.Equal(t, 15*time.Second, receive(t, children[2].waits))
	require.Equal(t, windowsServiceResult{false, windows.NO_ERROR}, receive(t, result))
}

func TestWindowsServiceStopsDuringRestartBackoff(t *testing.T) {
	child := newFakeWindowsServiceChild()
	spawned := make(chan *fakeWindowsServiceChild, 2)
	backoff := &blockingWindowsServiceBackoff{
		started: make(chan struct{}, 1),
		stopped: make(chan struct{}, 1),
		resets:  make(chan struct{}, 1),
	}
	windowsService := &service{
		spawnChild: func([]string, string, string, string) (windowsServiceChild, error) {
			spawned <- child
			return child, nil
		},
		newBackoff: func() windowsServiceBackoff { return backoff },
	}

	requests := make(chan svc.ChangeRequest, 1)
	statuses := make(chan svc.Status, 2)
	result := executeWindowsService(windowsService, requests, statuses)

	require.Same(t, child, receive(t, spawned))
	require.Equal(t, svc.Running, receive(t, statuses).State)

	child.done <- errors.New("daemon crashed")
	receive(t, child.crashes)
	receive(t, backoff.started)
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	require.Equal(t, svc.StopPending, receive(t, statuses).State)
	receive(t, backoff.stopped)
	require.Equal(t, windowsServiceResult{false, windows.NO_ERROR}, receive(t, result))
	select {
	case unexpected := <-spawned:
		t.Fatalf("spawned child during service stop: %p", unexpected)
	default:
	}
}

func TestWindowsServiceLogsChildShutdownError(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	shutdownErr := errors.New("child exited unsuccessfully")
	child := newFakeWindowsServiceChild()
	child.waitErr = shutdownErr
	windowsService := &service{
		spawnChild: func([]string, string, string, string) (windowsServiceChild, error) {
			return child, nil
		},
		newBackoff: func() windowsServiceBackoff { return immediateWindowsServiceBackoff{} },
	}

	requests := make(chan svc.ChangeRequest, 1)
	statuses := make(chan svc.Status, 2)
	result := executeWindowsService(windowsService, requests, statuses)

	require.Equal(t, svc.Running, receive(t, statuses).State)
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	require.Equal(t, svc.StopPending, receive(t, statuses).State)
	require.Equal(t, 15*time.Second, receive(t, child.waits))
	require.Equal(t, windowsServiceResult{false, windows.NO_ERROR}, receive(t, result))
	require.Contains(t, logs.String(), "Daemon process did not stop cleanly")
	require.Contains(t, logs.String(), shutdownErr.Error())
}

func TestWindowsServiceReturnsFailureWhenChildCannotRestart(t *testing.T) {
	child := newFakeWindowsServiceChild()
	spawnCalls := 0
	restartErr := errors.New("restart failed")
	windowsService := &service{
		spawnChild: func([]string, string, string, string) (windowsServiceChild, error) {
			spawnCalls++
			if spawnCalls == 1 {
				return child, nil
			}
			return nil, restartErr
		},
		newBackoff: func() windowsServiceBackoff { return immediateWindowsServiceBackoff{} },
	}

	requests := make(chan svc.ChangeRequest, 1)
	statuses := make(chan svc.Status, 1)
	result := executeWindowsService(windowsService, requests, statuses)

	require.Equal(t, svc.Running, receive(t, statuses).State)
	child.done <- restartErr
	require.ErrorIs(t, receive(t, child.crashes), restartErr)
	require.Equal(t, windowsServiceResult{true, 1}, receive(t, result))
	require.Equal(t, 2, spawnCalls)
}

type fakeWindowsServiceRecoveryConfigurer struct {
	actions         []mgr.RecoveryAction
	resetPeriod     uint32
	nonCrashFailure bool
}

func (f *fakeWindowsServiceRecoveryConfigurer) SetRecoveryActions(actions []mgr.RecoveryAction, resetPeriod uint32) error {
	f.actions = actions
	f.resetPeriod = resetPeriod
	return nil
}

func (f *fakeWindowsServiceRecoveryConfigurer) SetRecoveryActionsOnNonCrashFailures(flag bool) error {
	f.nonCrashFailure = flag
	return nil
}

func TestConfigureWindowsServiceRecovery(t *testing.T) {
	configured := &fakeWindowsServiceRecoveryConfigurer{}
	require.NoError(t, configureWindowsServiceRecovery(configured))
	require.Equal(t, uint32(60), configured.resetPeriod)
	require.Equal(t, []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 1 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 2 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 4 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 8 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 16 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 32 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 64 * time.Second},
	}, configured.actions)
	require.True(t, configured.nonCrashFailure)
}

func executeWindowsService(windowsService *service, requests chan svc.ChangeRequest, statuses chan svc.Status) <-chan windowsServiceResult {
	result := make(chan windowsServiceResult, 1)
	go func() {
		serviceSpecificExitCode, exitCode := windowsService.run(serviceRunConfig{
			dataPath:    `C:\ProgramData\Lantern`,
			logPath:     `C:\ProgramData\Lantern`,
			logLevel:    "debug",
			environment: daemonEnvironmentProd,
		}, requests, statuses)
		result <- windowsServiceResult{serviceSpecificExitCode, exitCode}
	}()
	return result
}

func receive[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for value")
		var zero T
		return zero
	}
}
