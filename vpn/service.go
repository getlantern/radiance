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
	"github.com/getlantern/radiance/servers"
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

// Start initializes and starts the tunnel with the specified options. Returns an error if the
// tunnel is already running or initialization fails.
func (s *TunnelService) Start(ctx context.Context, options string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tunnel != nil {
		s.logger.Warn("tunnel already started")
		return errors.New("tunnel already started")
	}
	s.logger.Debug("Starting tunnel", "options", options)
	if err := s.start(ctx, options); err != nil {
		return err
	}
	return nil
}

func (s *TunnelService) start(ctx context.Context, options string) error {
	path := settings.GetString(settings.DataPathKey)
	t := tunnel{
		dataPath: path,
	}
	if err := t.start(options, s.platformIfce); err != nil {
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
func (s *TunnelService) Restart(ctx context.Context, options string) error {
	s.mu.Lock()
	if s.tunnel == nil {
		s.mu.Unlock()
		return errors.New("tunnel not started")
	}
	if s.tunnel.Status() != ipc.Connected {
		s.mu.Unlock()
		return errors.New("tunnel not running")
	}

	s.logger.Info("Restarting tunnel")
	s.tunnel.setStatus(ipc.Restarting, nil)
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
	if err := s.start(ctx, options); err != nil {
		s.logger.Error("starting tunnel", "error", err)
		return fmt.Errorf("starting tunnel: %w", err)
	}
	s.logger.Info("Tunnel restarted successfully")
	return nil
}

// Status returns the current status of the tunnel (e.g., running, closed).
func (s *TunnelService) Status() ipc.VPNStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tunnel == nil {
		return ipc.Disconnected
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

var errTunnelNotStarted = errors.New("tunnel not started")

// activeTunnel returns the running tunnel or errTunnelNotStarted.
func (s *TunnelService) activeTunnel() (*tunnel, error) {
	s.mu.Lock()
	t := s.tunnel
	s.mu.Unlock()
	if t == nil {
		return nil, errTunnelNotStarted
	}
	return t, nil
}

func (s *TunnelService) UpdateOutbounds(newOpts servers.Servers) error {
	t, err := s.activeTunnel()
	if err != nil {
		return err
	}
	return t.updateOutbounds(newOpts)
}

func (s *TunnelService) AddOutbounds(group string, options servers.Options) error {
	t, err := s.activeTunnel()
	if err != nil {
		return err
	}
	return t.addOutbounds(group, options)
}

func (s *TunnelService) RemoveOutbounds(group string, tags []string) error {
	t, err := s.activeTunnel()
	if err != nil {
		return err
	}
	return t.removeOutbounds(group, tags)
}
