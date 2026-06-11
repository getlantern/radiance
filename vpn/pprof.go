package vpn

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

// pprofServerOnce binds the pprof server once per process. The tunnel is the
// only path guaranteed to run in whichever process hosts sing-box, which on
// macOS/iOS is the network-extension process — distinct from the control
// process that serves IPC. Binding from the control process profiled the
// wrong one: the proxy's cost, including the WATER WASM outbound, lives
// wherever libbox runs.
var pprofServerOnce sync.Once

// startPprofServer starts a loopback pprof/HTTP server when env.Pprof
// (RADIANCE_PPROF_ADDR) resolves to a non-empty address, and is a no-op
// otherwise so it ships safely in release builds. Call it from the sing-box
// process (see pprofServerOnce); profiling elsewhere misses the tunnel.
//
// The address is read through the radiance env package so it honours an OS
// env var, a .env file, or a runtime env.Set — the last being the only
// channel that reaches a sandboxed system extension, which inherits neither
// the launching shell's environment nor the repo's working directory.
func startPprofServer() {
	addr := env.GetString(env.Pprof)
	if addr == "" {
		return
	}
	if !isLoopbackAddr(addr) {
		slog.Error("Refusing to start pprof server on a non-loopback address",
			"addr", addr, "env", env.Pprof.String())
		return
	}

	// Validate addr before consuming the Once: an early call before the env
	// override is applied must not permanently disable the server.
	pprofServerOnce.Do(func() {
		// Dedicated mux: net/http/pprof registers on http.DefaultServeMux at
		// import, which would otherwise expose these handlers on any server in
		// the process serving the default mux.
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
		}()
	})
}

// isLoopbackAddr reports whether addr (a "host:port") binds only to the
// loopback interface. The empty host (e.g. ":6060") binds every interface
// and is rejected; "localhost" and explicit loopback IPs (127.0.0.0/8, ::1)
// are accepted.
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
