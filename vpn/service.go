package vpn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"

	"github.com/sagernet/sing-box/experimental/clashapi"
	"github.com/sagernet/sing-box/experimental/libbox"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/vpn/ipc"
)

var _ ipc.Service = (*TunnelService)(nil)

// TunnelService manages the lifecycle of the VPN tunnel.
type TunnelService struct {
	tunnel *tunnel

	platformIfce libbox.PlatformInterface
	dataPath     string
	logPath      string
	logLevel     string

	mu sync.Mutex
}

// NewTunnelService creates a new TunnelService instance with the provided configuration paths, log
// level, and platform interface.
func NewTunnelService(dataPath, logPath, logLevel string, platformIfce libbox.PlatformInterface) *TunnelService {
	return &TunnelService{
		platformIfce: platformIfce,
		dataPath:     dataPath,
		logPath:      logPath,
		logLevel:     logLevel,
	}
}

// Start initializes and starts the tunnel with the specified group and tag. Returns an error if the
// tunnel is already running or initialization fails.
func (s *TunnelService) Start(group string, tag string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tunnel != nil && s.tunnel.Status() != ipc.StatusClosed {
		return errors.New("tunnel already started")
	}
	if err := common.InitReadOnly(s.dataPath, s.logPath, s.logLevel); err != nil {
		return fmt.Errorf("initialize common package: %w", err)
	}
	s.dataPath = settings.GetString(settings.DataPathKey)

	_ = newSplitTunnel(s.dataPath)
	return s.start(group, tag)
}

func (s *TunnelService) start(group string, tag string) error {
	opts, err := buildOptions(group, s.dataPath)
	if err != nil {
		return fmt.Errorf("failed to build options: %w", err)
	}
	t := tunnel{
		dataPath: s.dataPath,
	}
	if err := t.start(group, tag, opts, s.platformIfce); err != nil {
		return fmt.Errorf("failed to start tunnel: %w", err)
	}
	s.tunnel = &t
	return nil
}

// Close shuts down the currently running tunnel, if any. Returns an error if closing the tunnel fails.
func (s *TunnelService) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tunnel == nil {
		return nil
	}
	t := s.tunnel
	s.tunnel = nil
	slog.Info("Closing tunnel")
	if err := t.close(); err != nil {
		return err
	}
	slog.Debug("Tunnel closed")
	return nil
}

// Restart closes and restarts the tunnel if it is currently running. Returns an error if the tunnel
// is not running or restart fails.
func (s *TunnelService) Restart() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tunnel == nil {
		return errors.New("tunnel not started")
	}
	if s.tunnel.Status() != ipc.StatusRunning {
		return errors.New("tunnel not running")
	}
	t := s.tunnel
	s.tunnel = nil
	group := t.clashServer.Mode()
	tag := t.cacheFile.LoadSelected(group)

	slog.Info("Restarting tunnel")
	if err := t.close(); err != nil {
		return fmt.Errorf("closing tunnel: %w", err)
	}
	runtime.GC()

	return s.start(group, tag)
}

// Status returns the current status of the tunnel (e.g., running, closed).
func (s *TunnelService) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tunnel == nil {
		return ipc.StatusClosed
	}
	return s.tunnel.Status()
}

// Ctx returns the context associated with the tunnel, or nil if no tunnel is running.
func (s *TunnelService) Ctx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tunnel == nil {
		return nil
	}
	return s.tunnel.ctx
}

// ClashServer returns the Clash server instance associated with the tunnel, or nil if no tunnel is
// running.
func (s *TunnelService) ClashServer() *clashapi.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tunnel == nil {
		return nil
	}
	return s.tunnel.clashServer
}
