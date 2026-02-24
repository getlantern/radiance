package vpn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/sagernet/sing-box/experimental/clashapi"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/vpn/ipc"
	"github.com/getlantern/radiance/vpn/rvpn"
)

var _ ipc.Service = (*TunnelService)(nil)

// TunnelService manages the lifecycle of the VPN tunnel.
type TunnelService struct {
	tunnel *tunnel

	platformIfce rvpn.PlatformInterface
	logger       *slog.Logger

	mu sync.Mutex
}

// NewTunnelService creates a new TunnelService instance with the provided configuration paths, log
// level, and platform interface.
func NewTunnelService(dataPath string, logger *slog.Logger, platformIfce rvpn.PlatformInterface) *TunnelService {
	if logger == nil {
		logger = slog.Default()
	}
	switch logger.Handler().(type) {
	case *slog.TextHandler, *slog.JSONHandler:
	default:
		os.MkdirAll(dataPath, 0o755)
		path := filepath.Join(dataPath, "radiance_vpn.log")
		var writer io.Writer
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			slog.Error("Failed to open log file", "error", err)
			writer = os.Stdout
		} else {
			writer = f
		}
		logger = slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{AddSource: true, Level: internal.LevelTrace}))
		runtime.AddCleanup(logger, func(file *os.File) {
			file.Close()
		}, f)
	}
	return &TunnelService{
		platformIfce: platformIfce,
		logger:       logger,
	}
}

// Start initializes and starts the tunnel with the specified group and tag. Returns an error if the
// tunnel is already running or initialization fails.
func (s *TunnelService) Start(ctx context.Context, group string, tag string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tunnel != nil {
		s.logger.Warn("tunnel already started")
		return errors.New("tunnel already started")
	}
	s.logger.Debug("Starting tunnel", "group", group, "tag", tag)
	if err := s.start(ctx); err != nil {
		return err
	}
	if group != "" {
		if err := s.tunnel.selectOutbound(group, tag); err != nil {
			slog.Error("Failed to select outbound", "group", group, "tag", tag, "error", err)
			return fmt.Errorf("selecting outbound: %w", err)
		}
	}
	return nil
}

func (s *TunnelService) start(ctx context.Context) error {
	path := settings.GetString(settings.DataPathKey)
	_ = newSplitTunnel(path)
	opts, err := buildOptions(ctx, path)
	if err != nil {
		return fmt.Errorf("failed to build options: %w", err)
	}
	t := tunnel{
		dataPath: path,
	}
	if err := t.start(opts, s.platformIfce); err != nil {
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
	if err := s.close(); err != nil {
		return err
	}
	if s.platformIfce != nil {
		s.platformIfce.PostServiceClose()
	}
	return nil
}

func (s *TunnelService) close() error {
	t := s.tunnel
	s.tunnel = nil

	s.logger.Info("Closing tunnel")
	if err := t.close(); err != nil {
		return err
	}
	s.logger.Debug("Tunnel closed")
	runtime.GC()
	return nil
}

// Restart closes and restarts the tunnel if it is currently running. Returns an error if the tunnel
// is not running or restart fails.
func (s *TunnelService) Restart(ctx context.Context) error {
	s.mu.Lock()
	if s.tunnel == nil {
		s.mu.Unlock()
		return errors.New("tunnel not started")
	}
	if s.tunnel.Status() != ipc.StatusRunning {
		s.mu.Unlock()
		return errors.New("tunnel not running")
	}

	s.logger.Info("Restarting tunnel")
	if s.platformIfce != nil {
		s.mu.Unlock()
		if err := s.platformIfce.RestartService(); err != nil {
			s.logger.Error("Failed to restart tunnel via platform interface", "error", err)
			return fmt.Errorf("platform interface restart failed: %w", err)
		}
		return nil
	}

	defer s.mu.Unlock()
	if err := s.close(); err != nil {
		return fmt.Errorf("closing tunnel: %w", err)
	}
	if err := s.start(ctx); err != nil {
		s.logger.Error("starting tunnel", "error", err)
		return fmt.Errorf("starting tunnel: %w", err)
	}
	s.logger.Info("Tunnel restarted successfully")
	return nil
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
