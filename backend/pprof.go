package backend

import (
	"errors"
	"log/slog"
	"net"
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
// Non-loopback addresses are refused outright rather than trusted to the
// caller: the pprof endpoints expose goroutine stacks and can be driven to
// consume CPU, so they must never be reachable off-device.
func (r *LocalBackend) startDebugServer() {
	addr := os.Getenv(pprofAddrEnv)
	if addr == "" {
		return
	}
	if !isLoopbackAddr(addr) {
		slog.Error("Refusing to start pprof server on a non-loopback address",
			"addr", addr, "env", pprofAddrEnv)
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
	// Tie the server to Close() so a Start/Close cycle (re-init, tests,
	// in-process clients) doesn't leak the goroutine or leave the port
	// bound — which would fail the next Start with "address already in
	// use". srv.Close aborts in-flight profile requests immediately, which
	// is what we want on shutdown.
	r.shutdownFuncs = append(r.shutdownFuncs, srv.Close)
	slog.Warn("Starting radiance pprof server (debug only)", "addr", addr)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("radiance pprof server stopped", "error", err)
		}
	}()
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
