package backend

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"time"
)

// pprofAddrEnv names the environment variable that opts a build into the
// on-device pprof server. Off by default: with the variable unset nothing
// is registered and no port is opened, so this is safe to ship in release
// builds. Set it to a loopback address (e.g. "localhost:6060") to profile.
const pprofAddrEnv = "RADIANCE_PPROF_ADDR"

// startDebugServer starts a loopback pprof/HTTP server when pprofAddrEnv is
// set. Radiance is compiled into the host app via gomobile, so there is no
// process to attach a profiler to and no other on-device profiling hook —
// this is the way to capture a CPU/heap profile of the running client,
// notably the broflake / Unbounded WebRTC relay, whose cost is otherwise
// invisible. Example:
//
//	RADIANCE_PPROF_ADDR=localhost:6060 <app>
//	go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
//
// Bind only to loopback: the pprof endpoints expose goroutine stacks and
// can force expensive profiles, so they must never be reachable off-device.
func startDebugServer() {
	addr := os.Getenv(pprofAddrEnv)
	if addr == "" {
		return
	}

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
	slog.Warn("Starting radiance pprof server (debug only)", "addr", addr)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("radiance pprof server stopped", "error", err)
		}
	}()
}
