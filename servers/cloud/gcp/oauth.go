package gcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/getlantern/radiance/servers/cloud/common"
)

const (
	callbackServerShutdownTimeout = 5 * time.Second // Graceful shutdown timeout
)

// closeWindowHTML generates simple HTML to close the browser window.
func closeWindowHTML(messageHTML string) string {
	return fmt.Sprintf(`<html><script>window.close()</script><body>%s. You can close this window.</body></html>`, messageHTML)
}

// RunOauth starts the DigitalOcean OAuth flow.
func RunOauth(openBrowser func(string) error) *common.OauthSession {
	ctx, cancel := context.WithCancel(context.Background())
	resultChan := make(chan common.OauthResult, 1) // Buffered channel size 1

	session := &common.OauthSession{
		Result: resultChan,
		Cancel: func() {
			// Make cancel idempotent
			select {
			case <-ctx.Done():
				// Already canceled or finished
			default:
				cancel()
				// Send a cancellation error if not already resolved
				select {
				case resultChan <- common.OauthResult{Err: errors.New("authentication cancelled")}:
				default:
					// Result channel already has a value (success or other error)
				}
			}
		},
	}

	go startOauthFlow(ctx, openBrowser, resultChan)

	return session
}

func startOauthFlow(ctx context.Context, openBrowser func(string) error, resultChan chan<- common.OauthResult) {
	listener, listenErr := net.Listen("tcp", "127.0.0.0:0") // Listen on any available port
	if listenErr != nil {
		resultChan <- common.OauthResult{Err: fmt.Errorf("failed to listen on port: %w", listenErr)}
		return
	}
	defer listener.Close() // Ensure listener is closed when done
	addr := listener.Addr().String()
	oauthConf := &oauth2.Config{
		ClientID:     gcpOAuthClientID,
		ClientSecret: gcpOAuthClientSecret,
		RedirectURL:  "http://" + addr,
		Scopes: []string{
			"https://www.googleapis.com/auth/userinfo.email",
			"https://www.googleapis.com/auth/compute",
			"https://www.googleapis.com/auth/cloudplatformprojects",
			"https://www.googleapis.com/auth/cloud-billing",
			"https://www.googleapis.com/auth/service.management",
			"https://www.googleapis.com/auth/cloud-platform.read-only",
		},
		Endpoint: google.Endpoint,
	}

	// --- Server Setup ---
	mux := http.NewServeMux()
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Use a WaitGroup to ensure server shutdown completes before function exits
	var wg sync.WaitGroup
	wg.Add(1) // For the server goroutine

	// Channel to signal successful token retrieval to trigger server shutdown
	shutdownSignal := make(chan struct{})

	// --- Handler for initial redirect from DO (GET /) ---
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// Check for cancellation before proceeding
		select {
		case <-ctx.Done():
			http.Error(w, closeWindowHTML("Authentication cancelled"), http.StatusServiceUnavailable)
			return
		default:
		}
		if r.URL.Query().Has("error") {
			if r.URL.Query().Get("error") == "access_denied" {
				http.Error(w, closeWindowHTML("Authentication cancelled"), http.StatusForbidden)
				resultChan <- common.OauthResult{Err: errors.New("authentication cancelled")}
			} else {
				http.Error(w, closeWindowHTML("Authentication failed"), http.StatusBadRequest)
				resultChan <- common.OauthResult{Err: fmt.Errorf("oauth error: %s", r.URL.Query().Get("error"))}
			}
			close(shutdownSignal) // Signal server shutdown
			return
		}
		token, err := oauthConf.Exchange(ctx, r.URL.Query().Get("code"))
		if err != nil {
			http.Error(w, closeWindowHTML("Authentication failed"), http.StatusBadRequest)
			resultChan <- common.OauthResult{Err: fmt.Errorf("failed to exchange code: %w", err)}
			close(shutdownSignal) // Signal server shutdown
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, closeWindowHTML("Authentication successful"))
		resultChan <- common.OauthResult{Token: token.RefreshToken}
		close(shutdownSignal) // Signal successful completion
	})

	// --- Start Server and Browser ---
	go func() {
		defer wg.Done() // Signal that the server goroutine has finished
		slog.Debug("Starting OAuth callback server...")
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Debug("OAuth callback server error", "err", err)
			// Try sending error only if context is not already canceled
			select {
			case <-ctx.Done(): // Already canceled, likely handled
			default:
				// Ensure the channel hasn't received a result already
				select {
				case resultChan <- common.OauthResult{Err: fmt.Errorf("callback server error: %w", err)}:
				default: // Already has a result, ignore this error
				}
			}
		}
		slog.Debug("OAuth callback server stopped.")
	}()

	// Redirect user to Google's consent page to ask for permission
	// for the scopes specified above.
	oauthURL := oauthConf.AuthCodeURL("state")

	slog.Debug("Opening browser", "url", oauthURL)
	err := openBrowser(oauthURL) // Use an external library to open a browser
	if err != nil {
		// Don't immediately fail, user can still copy-paste the URL
		slog.Info("Failed to automatically open browser", "err", err)
		slog.Info("Please manually open the following URL in your browser", "url", oauthURL)
	}

	// --- Wait for completion or cancellation ---
	select {
	case <-shutdownSignal:
		slog.Debug("OAuth flow completed, shutting down server...")
	case <-ctx.Done():
		slog.Debug("OAuth flow cancelled, shutting down server...")
		// The cancel func already sent the error via resultChan
	}

	// --- Shutdown Server Gracefully ---
	shutdownCtx, cancelShutdown := context.WithTimeout(ctx, callbackServerShutdownTimeout)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Debug("Error during OAuth server graceful shutdown", "err", err)
		// Force close if a graceful shutdown fails
		_ = server.Close()
	}

	wg.Wait() // Wait for the server goroutine to finish fully
	slog.Debug("OAuth flow cleanup complete.")
}
