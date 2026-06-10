package backend

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"sync"
	"time"

	"github.com/getlantern/radiance/common/env"
)

// debugServerOnce scopes the pprof server to the process. A LocalBackend is
// created and Closed on every VPN (re)connect, so tying the server to Close()
// left it bound only for the seconds between a connect and the next teardown.
var debugServerOnce sync.Once

// startDebugServer starts a loopback pprof/HTTP server when env.Pprof
// (RADIANCE_PPROF_ADDR) resolves to a non-empty address. Off by default:
// with nothing set, no handler is registered and no port is opened, so it
// ships safely in release builds.
//
// The address is read through the radiance env package, so it honours an
// OS env var, a .env file in the working dir, or a runtime env.Set (e.g.
// via the IPC SetEnv path). The latter two matter on sandboxed macOS/iOS
// system extensions, which don't inherit the launching shell's
// environment. Example:
//
//	echo 'RADIANCE_PPROF_ADDR=localhost:6060' >> .env   # or OS env / env.Set
//	go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
//
// Radiance is compiled into the host app via gomobile, so there is no
// process to attach a profiler to and no other on-device profiling hook —
// this is the way to capture a CPU/heap profile of the running client,
// notably the broflake / Unbounded WebRTC relay, whose cost is otherwise
// invisible.
//
// Non-loopback addresses are refused outright rather than trusted to the
// caller: the pprof endpoints expose goroutine stacks and can be driven to
// consume CPU, so they must never be reachable off-device.
func (r *LocalBackend) startDebugServer() {
	addr := env.GetString(env.Pprof)
	if addr == "" {
		return
	}
	if !isLoopbackAddr(addr) {
		slog.Error("Refusing to start pprof server on a non-loopback address",
			"addr", addr, "env", env.Pprof.String())
		return
	}

	// Validate addr before consuming the Once: an early Start() before the
	// env override is applied must not permanently disable the server.
	debugServerOnce.Do(func() {
		// Dedicated mux rather than http.DefaultServeMux, so importing
		// net/http/pprof here can't accidentally surface these handlers on any
		// other server in the process that happens to serve DefaultServeMux.
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

		srv := &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		slog.Info("Starting radiance pprof server (debug only)", "addr", addr)
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("radiance pprof server stopped", "error", err)
			}
			slog.Warn("pprof server stopped")
		}()
	})
}

// isLoopbackAddr reports whether addr (a "host:port") binds only to the
// loopback interface. The empty host (e.g. ":6060") binds every interface
// and is rejected; "localhost" and explicit loopback IPs (127.0.0.0/8,
// ::1) are accepted.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
