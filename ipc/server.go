// Package ipc implements the IPC server for communicating between the client and the VPN service.
// It provides HTTP endpoints for retrieving statistics, managing groups, selecting outbounds,
// changing modes, and closing connections.
package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/getlantern/radiance/account"
	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
	rlog "github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/vpn"

	sjson "github.com/sagernet/sing/common/json"
)

const (
	tracerName = "github.com/getlantern/radiance/ipc"

	// VPN endpoints
	vpnStatusEndpoint       = "/vpn/status"
	vpnConnectEndpoint      = "/vpn/connect"
	vpnDisconnectEndpoint   = "/vpn/disconnect"
	vpnRestartEndpoint      = "/vpn/restart"
	vpnConnectionsEndpoint  = "/vpn/connections"
	vpnOfflineTestsEndpoint = "/vpn/offline-tests"
	vpnStatusEventsEndpoint = "/vpn/status/events"

	// Server selection endpoints
	serverSelectedEndpoint           = "/server/selected"
	serverAutoSelectedEndpoint       = "/server/auto-selected"
	serverAutoSelectedEventsEndpoint = "/server/auto-selected/events"

	// Config endpoints
	configEventsEndpoint = "/config/events"
	configUpdateEndpoint = "/config/update"

	// Server management endpoints
	serversEndpoint              = "/servers"
	serversAddEndpoint           = "/servers/add"
	serversRemoveEndpoint        = "/servers/remove"
	serversFromJSONEndpoint      = "/servers/json"
	serversFromURLsEndpoint      = "/servers/urls"
	serversPrivateEndpoint       = "/servers/private"
	serversPrivateInviteEndpoint = "/servers/private/invite"

	// Settings endpoints
	featuresEndpoint = "/settings/features"
	settingsEndpoint = "/settings"

	// Split tunnel endpoint
	splitTunnelEndpoint = "/split-tunnel"

	// Account endpoints
	accountNewUserEndpoint       = "/account/new-user"
	accountLoginEndpoint         = "/account/login"
	accountLogoutEndpoint        = "/account/logout"
	accountUserDataEndpoint      = "/account/user"
	accountDevicesEndpoint       = "/account/devices/"
	accountSignupEndpoint        = "/account/signup/"
	accountEmailEndpoint         = "/account/email"
	accountRecoveryEndpoint      = "/account/recovery"
	accountDeleteEndpoint        = "/account/delete"
	accountOAuthEndpoint         = "/account/oauth"
	accountDataCapEndpoint       = "/account/datacap"
	accountDataCapStreamEndpoint = "/account/datacap/stream"

	// Subscription endpoints
	subscriptionActivationEndpoint         = "/subscription/activation"
	subscriptionStripeEndpoint             = "/subscription/stripe"
	subscriptionPaymentRedirectEndpoint    = "/subscription/payment-redirect"
	subscriptionReferralEndpoint           = "/subscription/referral"
	subscriptionBillingPortalEndpoint      = "/subscription/billing-portal"
	subscriptionPaymentRedirectURLEndpoint = "/subscription/payment-redirect-url"
	subscriptionPlansEndpoint              = "/subscription/plans"
	subscriptionVerifyEndpoint             = "/subscription/verify"

	// Issue endpoint
	issueEndpoint = "/issue"

	// Logs endpoint
	logsStreamEndpoint = "/logs/stream"

	// Env endpoint (dev/testing)
	envEndpoint = "/env"
)

var (
	protocols = func() http.Protocols {
		var p http.Protocols
		p.SetUnencryptedHTTP2(true)
		return p
	}()

	ErrServiceIsNotReady = errors.New("service is not ready")
	ErrIPCNotRunning     = errors.New("IPC not running")
)

// Server represents the IPC server that communicates over a Unix domain socket for Unix-like
// systems, and a named pipe for Windows.
type Server struct {
	svr    *http.Server
	closed atomic.Bool
}

// NewServer creates a new Server instance with the provided Backend.
func NewServer(b *backend.LocalBackend, withAuth bool) *Server {
	// Only add auth middleware if not running on mobile, since mobile platforms have their own
	// sandboxing and permission models.
	svr := &http.Server{
		Handler:     newLocalAPI(b, withAuth),
		ReadTimeout: 5 * time.Second,
		Protocols:   &protocols,
	}
	if withAuth {
		svr.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
			peer, err := getConnPeer(c)
			if err != nil {
				slog.Error("Failed to get peer credentials", "error", err)
			}
			return contextWithUsr(ctx, peer)
		}
	}
	return &Server{svr: svr}
}

// Start begins listening for incoming IPC requests.
func (s *Server) Start() error {
	if s.closed.Load() {
		return errors.New("IPC server is closed")
	}
	l, err := listen()
	if err != nil {
		return fmt.Errorf("IPC server: listen: %w", err)
	}
	go func() {
		slog.Info("IPC server started", "address", l.Addr().String())
		if err := s.svr.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("IPC server error", "error", err)
		}
		s.closed.Store(true)
	}()
	return nil
}

// Close shuts down the IPC server.
func (s *Server) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	slog.Info("Closing IPC server")
	return s.svr.Close()
}

type backendKey struct{}

type localapi struct {
	be      atomic.Pointer[backend.LocalBackend]
	handler http.Handler
}

// backend returns the LocalBackend snapshotted at the start of the request.
func (s *localapi) backend(ctx context.Context) *backend.LocalBackend {
	return ctx.Value(backendKey{}).(*backend.LocalBackend)
}

func newLocalAPI(b *backend.LocalBackend, withAuth bool) *localapi {
	s := &localapi{}
	s.be.Store(b)

	mux := http.NewServeMux()

	// traced wraps a handler with the tracer middleware.
	traced := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			tracer(http.HandlerFunc(h)).ServeHTTP(w, r)
		}
	}

	// VPN
	mux.HandleFunc("GET "+vpnStatusEndpoint, traced(s.vpnStatusHandler))
	mux.HandleFunc("POST "+vpnConnectEndpoint, traced(s.vpnConnectHandler))
	mux.HandleFunc("POST "+vpnDisconnectEndpoint, traced(s.vpnDisconnectHandler))
	mux.HandleFunc("POST "+vpnRestartEndpoint, traced(s.vpnRestartHandler))
	mux.HandleFunc("GET "+vpnConnectionsEndpoint, traced(s.vpnConnectionsHandler))
	mux.HandleFunc("POST "+vpnOfflineTestsEndpoint, traced(s.vpnOfflineTestsHandler))

	// SSE routes skip the tracer middleware since it buffers the entire response body.
	mux.HandleFunc("GET "+vpnStatusEventsEndpoint, s.vpnStatusEventsHandler)

	// Server selection
	mux.HandleFunc(serverSelectedEndpoint, traced(s.serverSelectedHandler))
	mux.HandleFunc("GET "+serverAutoSelectedEndpoint, traced(s.serverAutoSelectedHandler))
	mux.HandleFunc("GET "+serverAutoSelectedEventsEndpoint, s.serverAutoSelectedEventsHandler)
	mux.HandleFunc("GET "+configEventsEndpoint, s.configEventsHandler)
	mux.HandleFunc("POST "+configUpdateEndpoint, traced(s.configUpdateHandler))

	// Server management
	mux.HandleFunc("GET "+serversEndpoint, traced(s.serversHandler))
	mux.HandleFunc("POST "+serversAddEndpoint, traced(s.serversAddHandler))
	mux.HandleFunc("POST "+serversRemoveEndpoint, traced(s.serversRemoveHandler))
	mux.HandleFunc("POST "+serversFromJSONEndpoint, traced(s.serversFromJSONHandler))
	mux.HandleFunc("POST "+serversFromURLsEndpoint, traced(s.serversFromURLsHandler))
	mux.HandleFunc("POST "+serversPrivateEndpoint, traced(s.serversPrivateAddHandler))
	mux.HandleFunc(serversPrivateInviteEndpoint, traced(s.serversPrivateInviteHandler))

	// Settings
	mux.HandleFunc("GET "+featuresEndpoint, traced(s.featuresHandler))
	mux.HandleFunc(settingsEndpoint, traced(s.settingsHandler))

	// Split tunnel
	mux.HandleFunc(splitTunnelEndpoint, traced(s.splitTunnelHandler))

	// Account
	mux.HandleFunc("POST "+accountNewUserEndpoint, traced(s.accountNewUserHandler))
	mux.HandleFunc("POST "+accountLoginEndpoint, traced(s.accountLoginHandler))
	mux.HandleFunc("POST "+accountLogoutEndpoint, traced(s.accountLogoutHandler))
	mux.HandleFunc("GET "+accountUserDataEndpoint, traced(s.accountUserDataHandler))
	mux.HandleFunc(accountDevicesEndpoint+"{deviceID...}", traced(s.accountDevicesHandler))
	mux.HandleFunc("POST "+accountSignupEndpoint+"{action...}", traced(s.accountSignupHandler))
	mux.HandleFunc("POST "+accountEmailEndpoint+"/{action}", traced(s.accountEmailHandler))
	mux.HandleFunc("POST "+accountRecoveryEndpoint+"/{action}", traced(s.accountRecoveryHandler))
	mux.HandleFunc("DELETE "+accountDeleteEndpoint, traced(s.accountDeleteHandler))
	mux.HandleFunc(accountOAuthEndpoint, traced(s.accountOAuthHandler))
	mux.HandleFunc("GET "+accountDataCapEndpoint, traced(s.accountDataCapHandler))

	// SSE routes skip the tracer middleware since it buffers the entire response body.
	mux.HandleFunc("GET "+accountDataCapStreamEndpoint, s.accountDataCapStreamHandler)

	// Subscriptions
	mux.HandleFunc("POST "+subscriptionActivationEndpoint, traced(s.subscriptionActivationHandler))
	mux.HandleFunc("POST "+subscriptionStripeEndpoint, traced(s.subscriptionStripeHandler))
	mux.HandleFunc("POST "+subscriptionPaymentRedirectEndpoint, traced(s.subscriptionPaymentRedirectHandler))
	mux.HandleFunc("POST "+subscriptionReferralEndpoint, traced(s.subscriptionReferralHandler))
	mux.HandleFunc("GET "+subscriptionBillingPortalEndpoint, traced(s.subscriptionBillingPortalHandler))
	mux.HandleFunc("POST "+subscriptionPaymentRedirectURLEndpoint, traced(s.subscriptionPaymentRedirectURLHandler))
	mux.HandleFunc("GET "+subscriptionPlansEndpoint, traced(s.subscriptionPlansHandler))
	mux.HandleFunc("POST "+subscriptionVerifyEndpoint, traced(s.subscriptionVerifyHandler))

	// Issue
	mux.HandleFunc("POST "+issueEndpoint, traced(s.issueReportHandler))

	// Logs (SSE, skip tracer)
	mux.HandleFunc("GET "+logsStreamEndpoint, s.logsStreamHandler)

	// Env (dev/testing)
	mux.HandleFunc(envEndpoint, traced(s.envHandler))

	// Build the middleware chain: log -> (optional auth) -> mux
	var handler http.Handler = mux
	if withAuth {
		handler = authPeer(handler)
	}
	handler = logger(handler)
	s.handler = handler

	return s
}

func (s *localapi) setBackend(b *backend.LocalBackend) *backend.LocalBackend {
	return s.be.Swap(b)
}

func (s *localapi) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b := s.be.Load()
	if b == nil {
		http.Error(w, "service is not ready", http.StatusServiceUnavailable)
		return
	}
	ctx := context.WithValue(r.Context(), backendKey{}, b)
	s.handler.ServeHTTP(w, r.WithContext(ctx))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("IPC: failed to write JSON response", "error", err)
	}
}

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func writeSingJSON[T any](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := sjson.NewEncoderContext(boxCtx, w).Encode(v); err != nil {
		slog.Error("IPC: failed to write JSON response", "error", err)
	}
}

func decodeSingJSON(r *http.Request, v any) error {
	return sjson.NewDecoderContext(boxCtx, r.Body).Decode(v)
}

// sseWriter sets headers for a Server-Sent Events response and returns the flusher.
// Returns nil if the ResponseWriter does not support flushing.
func sseWriter(w http.ResponseWriter) http.Flusher {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return flusher
}

/////////////
//   VPN   //
/////////////

func (s *localapi) vpnStatusHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backend(r.Context()).VPNStatus())
}

func (s *localapi) vpnConnectHandler(w http.ResponseWriter, r *http.Request) {
	var req TagRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.backend(r.Context()).ConnectVPN(req.Tag); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *localapi) vpnDisconnectHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.backend(r.Context()).DisconnectVPN(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *localapi) vpnRestartHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.backend(r.Context()).RestartVPN(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// vpnConnectionsHandler handles GET /vpn/connections/ (all) and GET /vpn/connections/active.
func (s *localapi) vpnConnectionsHandler(w http.ResponseWriter, r *http.Request) {
	var (
		conns []vpn.Connection
		err   error
	)
	if r.URL.Query().Get("active") == "true" {
		conns, err = s.backend(r.Context()).ActiveVPNConnections()
	} else {
		conns, err = s.backend(r.Context()).VPNConnections()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, conns)
}

func (s *localapi) vpnOfflineTestsHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.backend(r.Context()).RunOfflineURLTests(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *localapi) vpnStatusEventsHandler(w http.ResponseWriter, r *http.Request) {
	flusher := sseWriter(w)
	if flusher == nil {
		return
	}
	ch := make(chan []byte, 16)
	sub := events.Subscribe(func(evt vpn.StatusUpdateEvent) {
		data, err := json.Marshal(evt)
		if err != nil {
			return
		}
		select {
		case ch <- data:
		default:
		}
	})
	defer sub.Unsubscribe()
	for {
		select {
		case data := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

///////////////////////
// Server selection  //
///////////////////////

// serverSelectedHandler handles GET /server/selected (read) and POST /server/selected (set).
func (s *localapi) serverSelectedHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req TagRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.backend(r.Context()).SelectServer(req.Tag); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	server, exists, err := s.backend(r.Context()).SelectedServer()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeSingJSON(w, http.StatusOK, SelectedServerResponse{Server: server, Exists: exists})
}

func (s *localapi) serverAutoSelectedHandler(w http.ResponseWriter, r *http.Request) {
	tag, err := s.backend(r.Context()).CurrentAutoSelectedServer()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	server, found := s.backend(r.Context()).GetServerByTag(tag)
	if !found {
		http.Error(w, "auto-selected server not found", http.StatusNotFound)
		return
	}
	writeSingJSON(w, http.StatusOK, server)
}

func (s *localapi) serverAutoSelectedEventsHandler(w http.ResponseWriter, r *http.Request) {
	flusher := sseWriter(w)
	if flusher == nil {
		return
	}
	ch := make(chan []byte, 16)
	sub := events.Subscribe(func(evt vpn.AutoSelectedEvent) {
		data, err := json.Marshal(evt)
		if err != nil {
			return
		}
		select {
		case ch <- data:
		default:
		}
	})
	defer sub.Unsubscribe()
	for {
		select {
		case data := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *localapi) configUpdateHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.backend(r.Context()).UpdateConfig(); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, config.ErrConfigFetchDisabled) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// configEventsHandler streams a notification on every config.NewConfigEvent.
// The payload is always "{}" — subscribers only need to know a change
// occurred and fetch fresh state through the other GET endpoints, so we don't
// serialize the (potentially large) full Config.
func (s *localapi) configEventsHandler(w http.ResponseWriter, r *http.Request) {
	flusher := sseWriter(w)
	if flusher == nil {
		return
	}
	ch := make(chan struct{}, 16)
	sub := events.Subscribe(func(evt config.NewConfigEvent) {
		select {
		case ch <- struct{}{}:
		default:
		}
	})
	defer sub.Unsubscribe()
	for {
		select {
		case <-ch:
			fmt.Fprint(w, "data: {}\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

///////////////////////
// Server management //
///////////////////////

// serversHandler handles GET /servers
func (s *localapi) serversHandler(w http.ResponseWriter, r *http.Request) {
	if tag := r.URL.Query().Get("tag"); tag != "" {
		server, found := s.backend(r.Context()).GetServerByTag(tag)
		if !found {
			http.Error(w, "server not found", http.StatusNotFound)
			return
		}
		writeSingJSON(w, http.StatusOK, server)
		return
	}
	writeSingJSON(w, http.StatusOK, s.backend(r.Context()).AllServers())
}

func (s *localapi) serversAddHandler(w http.ResponseWriter, r *http.Request) {
	var req AddServersRequest
	if err := decodeSingJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.backend(r.Context()).AddServers(req.Servers); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *localapi) serversRemoveHandler(w http.ResponseWriter, r *http.Request) {
	var req RemoveServersRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.backend(r.Context()).RemoveServers(req.Tags); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *localapi) serversFromJSONHandler(w http.ResponseWriter, r *http.Request) {
	var req JSONConfigRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tags, err := s.backend(r.Context()).AddServersByJSON(req.Config)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, tags)
}

func (s *localapi) serversFromURLsHandler(w http.ResponseWriter, r *http.Request) {
	var req URLsRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tags, err := s.backend(r.Context()).AddServersByURL(req.URLs, req.SkipCertVerification)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, tags)
}

func (s *localapi) serversPrivateAddHandler(w http.ResponseWriter, r *http.Request) {
	var req PrivateServerRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	err := s.backend(r.Context()).AddPrivateServer(req.Tag, req.IP, req.Port, req.AccessToken, req.Location, req.Joined)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// serversPrivateInviteHandler handles POST (create) and DELETE (revoke) on /servers/private/invite.
func (s *localapi) serversPrivateInviteHandler(w http.ResponseWriter, r *http.Request) {
	var req PrivateServerInviteRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.backend(r.Context()).RevokePrivateServerInvite(req.IP, req.Port, req.AccessToken, req.InviteName); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	code, err := s.backend(r.Context()).InviteToPrivateServer(req.IP, req.Port, req.AccessToken, req.InviteName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, CodeResponse{Code: code})
}

//////////////
// Settings //
//////////////

func (s *localapi) featuresHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backend(r.Context()).Features())
}

func (s *localapi) settingsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPatch:
		var updates settings.Settings
		if err := decodeJSON(r, &updates); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.backend(r.Context()).PatchSettings(updates); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fallthrough
	case http.MethodGet:
		writeJSON(w, http.StatusOK, settings.GetAll())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *localapi) envHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPatch:
		var updates map[string]string
		if err := decodeJSON(r, &updates); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for k, v := range updates {
			env.Set(k, v)
		}
		fallthrough
	case http.MethodGet:
		writeJSON(w, http.StatusOK, env.GetAll())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

/////////////////
// Split Tunnel //
/////////////////

// splitTunnelHandler handles GET (read), POST (add), and DELETE (remove) on /split-tunnel.
func (s *localapi) splitTunnelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, s.backend(r.Context()).SplitTunnelFilters())
		return
	}
	var items vpn.SplitTunnelFilter
	if err := decodeJSON(r, &items); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var err error
	switch r.Method {
	case http.MethodPost:
		err = s.backend(r.Context()).AddSplitTunnelItems(items)
	case http.MethodDelete:
		err = s.backend(r.Context()).RemoveSplitTunnelItems(items)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

/////////////
// Account //
/////////////

func (s *localapi) accountNewUserHandler(w http.ResponseWriter, r *http.Request) {
	userData, err := s.backend(r.Context()).NewUser(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, userData)
}

func (s *localapi) accountLoginHandler(w http.ResponseWriter, r *http.Request) {
	var req EmailPasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userData, err := s.backend(r.Context()).Login(r.Context(), req.Email, req.Password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, userData)
}

func (s *localapi) accountLogoutHandler(w http.ResponseWriter, r *http.Request) {
	var req EmailRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userData, err := s.backend(r.Context()).Logout(r.Context(), req.Email)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, userData)
}

func (s *localapi) accountUserDataHandler(w http.ResponseWriter, r *http.Request) {
	var userData *account.UserData
	var err error
	if r.URL.Query().Get("fetch") == "true" {
		userData, err = s.backend(r.Context()).FetchUserData(r.Context())
	} else {
		userData, err = s.backend(r.Context()).UserData()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, userData)
}

// accountDevicesHandler handles GET /account/devices (list) and DELETE /account/devices/{deviceID} (remove).
func (s *localapi) accountDevicesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		resp, err := s.backend(r.Context()).RemoveDevice(r.Context(), r.PathValue("deviceID"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	devices, err := s.backend(r.Context()).UserDevices()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, devices)
}

// accountSignupHandler handles POST /account/signup, /account/signup/confirm, and /account/signup/resend.
func (s *localapi) accountSignupHandler(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("action") {
	case "confirm":
		var req EmailCodeRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.backend(r.Context()).SignupEmailConfirmation(r.Context(), req.Email, req.Code); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	case "resend":
		var req EmailRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.backend(r.Context()).SignupEmailResendCode(r.Context(), req.Email); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		var req EmailPasswordRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		salt, resp, err := s.backend(r.Context()).SignUp(r.Context(), req.Email, req.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, SignupResponse{Salt: salt, Response: resp})
	}
}

// accountEmailHandler handles POST /account/email/{action} for start and complete.
func (s *localapi) accountEmailHandler(w http.ResponseWriter, r *http.Request) {
	var err error
	switch r.PathValue("action") {
	case "start":
		var req ChangeEmailStartRequest
		if err = decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err = s.backend(r.Context()).StartChangeEmail(r.Context(), req.NewEmail, req.Password)
	case "complete":
		var req ChangeEmailCompleteRequest
		if err = decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err = s.backend(r.Context()).CompleteChangeEmail(r.Context(), req.NewEmail, req.Password, req.Code)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// accountRecoveryHandler handles POST /account/recovery/{action} for start, complete, and validate.
func (s *localapi) accountRecoveryHandler(w http.ResponseWriter, r *http.Request) {
	var err error
	switch r.PathValue("action") {
	case "start":
		var req EmailRequest
		if err = decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err = s.backend(r.Context()).StartRecoveryByEmail(r.Context(), req.Email)
	case "complete":
		var req RecoveryCompleteRequest
		if err = decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err = s.backend(r.Context()).CompleteRecoveryByEmail(r.Context(), req.Email, req.NewPassword, req.Code)
	case "validate":
		var req EmailCodeRequest
		if err = decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err = s.backend(r.Context()).ValidateEmailRecoveryCode(r.Context(), req.Email, req.Code)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *localapi) accountDeleteHandler(w http.ResponseWriter, r *http.Request) {
	var req EmailPasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userData, err := s.backend(r.Context()).DeleteAccount(r.Context(), req.Email, req.Password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, userData)
}

// accountOAuthHandler handles GET /account/oauth (login URL) and POST /account/oauth (callback).
func (s *localapi) accountOAuthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req OAuthTokenRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		userData, err := s.backend(r.Context()).OAuthLoginCallback(r.Context(), req.OAuthToken)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, userData)
		return
	}
	provider := r.URL.Query().Get("provider")
	if provider == "" {
		http.Error(w, "provider is required", http.StatusBadRequest)
		return
	}
	u, err := s.backend(r.Context()).OAuthLoginURL(r.Context(), provider)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, URLResponse{URL: u})
}

func (s *localapi) accountDataCapHandler(w http.ResponseWriter, r *http.Request) {
	info, err := s.backend(r.Context()).DataCapInfo(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *localapi) accountDataCapStreamHandler(w http.ResponseWriter, r *http.Request) {
	flusher := sseWriter(w)
	if flusher == nil {
		return
	}
	ch := s.backend(r.Context()).DataCapUpdates()
	for {
		select {
		case info := <-ch:
			data, err := json.Marshal(info)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

///////////////////
// Subscriptions //
///////////////////

func (s *localapi) subscriptionActivationHandler(w http.ResponseWriter, r *http.Request) {
	var req ActivationRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := s.backend(r.Context()).ActivationCode(r.Context(), req.Email, req.ResellerCode)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *localapi) subscriptionStripeHandler(w http.ResponseWriter, r *http.Request) {
	var req StripeSubscriptionRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	clientSecret, err := s.backend(r.Context()).NewStripeSubscription(r.Context(), req.Email, req.PlanID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, ClientSecretResponse{ClientSecret: clientSecret})
}

func (s *localapi) subscriptionPaymentRedirectHandler(w http.ResponseWriter, r *http.Request) {
	var req account.PaymentRedirectData
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u, err := s.backend(r.Context()).PaymentRedirect(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, URLResponse{URL: u})
}

func (s *localapi) subscriptionReferralHandler(w http.ResponseWriter, r *http.Request) {
	var req CodeRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ok, err := s.backend(r.Context()).ReferralAttach(r.Context(), req.Code)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, SuccessResponse{Success: ok})
}

func (s *localapi) subscriptionBillingPortalHandler(w http.ResponseWriter, r *http.Request) {
	u, err := s.backend(r.Context()).StripeBillingPortalURL(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, URLResponse{URL: u})
}

func (s *localapi) subscriptionPaymentRedirectURLHandler(w http.ResponseWriter, r *http.Request) {
	var req account.PaymentRedirectData
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u, err := s.backend(r.Context()).SubscriptionPaymentRedirectURL(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, URLResponse{URL: u})
}

func (s *localapi) subscriptionPlansHandler(w http.ResponseWriter, r *http.Request) {
	plans, err := s.backend(r.Context()).SubscriptionPlans(r.Context(), r.URL.Query().Get("channel"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, PlansResponse{Plans: plans})
}

func (s *localapi) subscriptionVerifyHandler(w http.ResponseWriter, r *http.Request) {
	var req VerifySubscriptionRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result, err := s.backend(r.Context()).VerifySubscription(r.Context(), req.Service, req.Data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, ResultResponse{Result: result})
}

///////////
// Issue //
///////////

func (s *localapi) issueReportHandler(w http.ResponseWriter, r *http.Request) {
	var req IssueReportRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.backend(r.Context()).ReportIssue(req.IssueType, req.Description, req.Email, req.AdditionalAttachments); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

///////////
// Logs  //
///////////

func (s *localapi) logsStreamHandler(w http.ResponseWriter, r *http.Request) {
	flusher := sseWriter(w)
	if flusher == nil {
		return
	}
	ch, unsub := rlog.Subscribe()
	defer unsub()
	for {
		select {
		case entry := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", entry)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
