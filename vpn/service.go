package vpn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sync"

	"github.com/sagernet/sing-box/experimental/clashapi"
	"github.com/sagernet/sing-box/experimental/libbox"

	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/vpn/ipc"
)

var _ ipc.Service = (*TunnelService)(nil)

// TunnelService manages the lifecycle of the VPN tunnel.
type TunnelService struct {
	tunnel *tunnel

	platformIfce libbox.PlatformInterface
	dataPath     string
	logger       *slog.Logger

	mu sync.Mutex
}

// NewTunnelService creates a new TunnelService instance with the provided configuration paths, log
// level, and platform interface.
func NewTunnelService(dataPath string, logger *slog.Logger, platformIfce libbox.PlatformInterface) *TunnelService {
	if logger == nil {
		logger = slog.Default()
	}
	switch logger.Handler().(type) {
	case *slog.TextHandler, *slog.JSONHandler:
	default:
		os.MkdirAll(dataPath, 0o755)
		f, err := os.OpenFile("radiance_vpn.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			slog.Error("Failed to open log file", "error", err)
			return nil
		}
		logger = slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{AddSource: true, Level: internal.LevelTrace}))
	}
	return &TunnelService{
		platformIfce: platformIfce,
		dataPath:     dataPath,
		logger:       logger,
	}
}

// Start initializes and starts the tunnel with the specified group and tag. Returns an error if the
// tunnel is already running or initialization fails.
func (s *TunnelService) Start(group string, tag string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tunnel != nil && s.tunnel.Status() != ipc.StatusClosed {
		s.logger.Warn("tunnel already started")
		return errors.New("tunnel already started")
	}
	s.logger.Debug("Starting tunnel", "group", group, "tag", tag)
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
	s.logger.Info("Closing tunnel")
	if err := t.close(); err != nil {
		return err
	}
	s.logger.Debug("Tunnel closed")
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

	s.logger.Info("Restarting tunnel")
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
