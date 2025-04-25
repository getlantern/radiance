package digitalocean

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/getlantern/radiance/servers/cloud/common"
)

// RegisteredRedirect represents a pre-registered client ID and port combination.
type RegisteredRedirect struct {
	ClientID string
	Port     int
}

var registeredRedirects = []RegisteredRedirect{
	{
		ClientID: "eb5975ebc602f0febfadee0ab5d50a1cd0b71bb6cc7b2c407256f6e78b8e5722",
		Port:     55189,
	},
	{
		ClientID: "e509d7bf1fe2c58b3e2e8cdec55d654c08f6b491ff4e5ca68b3e99794bdd73cc",
		Port:     60434,
	},
	{
		ClientID: "8e526379b60d2b924ac662dbf365a419410c5bf1e483d20315077f7e028ef2bc",
		Port:     61437,
	},
}

const (
	callbackServerShutdownTimeout = 5 * time.Second // Graceful shutdown timeout
	apiUserAgent                  = "Lantern Radiance"
	digitalOceanAPIEndpoint       = "https://api.digitalocean.com/v2/account"
	digitalOceanAuthEndpoint      = "https://cloud.digitalocean.com/v1/oauth/authorize"
)

// randomHex generates a random hexadecimal string of the specified length.
func randomHex(n int) (string, error) {
	bytes := make([]byte, (n+1)/2) // n+1 to handle odd lengths
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes)[:n], nil
}

// DOAccount represents the structure of the DigitalOcean user account info.
// See https://developers.digitalocean.com/documentation/v2/#get-user-information
type DOAccount struct {
	DropletLimit    int    `json:"droplet_limit"`
	FloatingIPLimit int    `json:"floating_ip_limit"`
	Email           string `json:"email"`
	UUID            string `json:"uuid"`
	EmailVerified   bool   `json:"email_verified"`
	Status          string `json:"status"`
	StatusMessage   string `json:"status_message"`
}

// AccountResponse is the top-level structure from the API.
type AccountResponse struct {
	Account DOAccount `json:"account"`
}

// GetAccount queries the DigitalOcean API for the user account information.
func GetAccount(accessToken string) (*DOAccount, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", digitalOceanAPIEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", apiUserAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to perform request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("api request failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var accountResp AccountResponse
	if err := json.Unmarshal(bodyBytes, &accountResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response JSON: %w", err)
	}

	return &accountResp.Account, nil
}

// closeWindowHTML generates simple HTML to close the browser window.
func closeWindowHTML(messageHTML string) string {
	return fmt.Sprintf(`<html><head><title>Authentication Status</title></head><script>window.onload=function(){setTimeout(function(){window.close()}, 100);}</script><body>%s. You can close this window.</body></html>`, messageHTML)
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
	secret, err := randomHex(16)
	if err != nil {
		resultChan <- common.OauthResult{Err: fmt.Errorf("failed to generate state secret: %w", err)}
		return
	}

	var listener net.Listener
	var chosenRedirect RegisteredRedirect
	var listenErr error

	// Try ports sequentially
	for _, redirect := range registeredRedirects {
		addr := fmt.Sprintf("localhost:%d", redirect.Port)
		slog.Debug("Attempting to listen", "addr", addr)
		listener, listenErr = net.Listen("tcp", addr)
		if listenErr == nil {
			chosenRedirect = redirect
			slog.Debug("Successfully listening", "port", chosenRedirect.Port)
			break // Found a port
		}

		// Check if the error is "address already in use"
		var opErr *net.OpError
		if errors.As(listenErr, &opErr) {
			var sysErr *os.SyscallError
			if errors.As(opErr.Err, &sysErr) {
				if errors.Is(sysErr.Err, syscall.EADDRINUSE) {
					slog.Debug("Port already in use", "port", redirect.Port)
					continue // Try the next port
				}
			}
		}

		// Other listening error, stop trying
		resultChan <- common.OauthResult{Err: fmt.Errorf("failed to listen on %s: %w", addr, listenErr)}
		return
	}

	if listener == nil {
		resultChan <- common.OauthResult{Err: errors.New("all registered OAuth ports are in use")}
		return
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

		// Serve the HTML with JavaScript to capture the fragment and POST it back
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, `<html>
          <head><title>Authenticating...</title></head>
          <body>
              <noscript>You need to enable JavaScript for DigitalOcean authentication.</noscript>
              <form id="form" method="POST">
                  <input id="params" type="hidden" name="params"></input>
              </form>
              <script>
                  if (window.location.hash) {
                      var paramsStr = location.hash.substr(1); // Remove leading '#'
                      var form = document.getElementById("form");
                      document.getElementById("params").setAttribute("value", paramsStr);
                      form.submit();
                  } else {
                      document.body.innerHTML = "Authentication failed: No parameters found in URL fragment.";
                  }
              </script>
          </body>
      </html>`)
	})

	// --- Handler for the POST back from the JavaScript ---
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) { // Handle POST to root
		// Check for cancellation before proceeding
		select {
		case <-ctx.Done():
			http.Error(w, closeWindowHTML("Authentication cancelled"), http.StatusServiceUnavailable)
			return
		default:
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, closeWindowHTML("Authentication failed: could not parse form"), http.StatusBadRequest)
			resultChan <- common.OauthResult{Err: fmt.Errorf("failed to parse form: %w", err)}
			close(shutdownSignal) // Signal server shutdown
			return
		}

		paramsStr := r.FormValue("params")
		if paramsStr == "" {
			http.Error(w, closeWindowHTML("Authentication failed: missing parameters"), http.StatusBadRequest)
			resultChan <- common.OauthResult{Err: errors.New("missing 'params' in form post")}
			close(shutdownSignal)
			return
		}

		// Parse the parameters originally from the fragment
		params, err := url.ParseQuery(paramsStr)
		if err != nil {
			http.Error(w, closeWindowHTML("Authentication failed: could not parse params"), http.StatusBadRequest)
			resultChan <- common.OauthResult{Err: fmt.Errorf("failed to parse fragment parameters: %w", err)}
			close(shutdownSignal)
			return
		}

		// Check for OAuth error response
		if errMsg := params.Get("error"); errMsg != "" {
			errDesc := params.Get("error_description")
			http.Error(w, closeWindowHTML("Authentication failed"), http.StatusBadRequest)
			resultChan <- common.OauthResult{Err: fmt.Errorf("digitalocean oauth error: %s (%s)", errMsg, errDesc)}
			close(shutdownSignal)
			return
		}

		// Validate state
		requestSecret := params.Get("state")
		if requestSecret != secret {
			http.Error(w, closeWindowHTML("Authentication failed: state mismatch"), http.StatusBadRequest)
			resultChan <- common.OauthResult{Err: fmt.Errorf("state mismatch: expected %s, got %s", secret, requestSecret)}
			close(shutdownSignal)
			return
		}

		// Get an access token
		accessToken := params.Get("access_token")
		if accessToken == "" {
			http.Error(w, closeWindowHTML("Authentication failed: no access token"), http.StatusBadRequest)
			resultChan <- common.OauthResult{Err: errors.New("no access_token found in oauth response")}
			close(shutdownSignal)
			return
		}

		// Optional: Verify token by fetching account info
		account, err := GetAccount(accessToken)
		if err != nil {
			slog.Debug("Warning: Failed to verify token by fetching account info", "err", err)
			// Decide if this should be a hard error or just a warning
			// For now, proceed but log it. Could send error:
			// http.Error(w, closeWindowHTML("Authentication failed: token verification failed"), http.StatusInternalServerError)
			// resultChan <- common.OauthResult{Err: fmt.Errorf("failed to verify token: %w", err)}
			// close(shutdownSignal)
			// return
		} else {
			slog.Debug("Successfully fetched account info", "user", account.Email, "status", account.Status)
			// Handle non-active accounts - redirect them to DO to finish setup
			if account.Status != "active" {
				slog.Debug("Redirecting to DigitalOcean Cloud.", "status", account.Status)
				http.Redirect(w, r, "https://cloud.digitalocean.com", http.StatusFound)
				// Still resolve with the token, manager might handle inactive accounts
				resultChan <- common.OauthResult{Token: accessToken}
				close(shutdownSignal)
				return
			}
		}

		// Success!
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, closeWindowHTML("Authentication successful"))
		resultChan <- common.OauthResult{Token: accessToken}
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

	// Construct OAuth URL
	oauthURL := fmt.Sprintf("%s?client_id=%s&response_type=token&scope=read%%20write&redirect_uri=http://localhost:%d/&state=%s",
		digitalOceanAuthEndpoint,
		url.QueryEscape(chosenRedirect.ClientID),
		chosenRedirect.Port,
		url.QueryEscape(secret),
	)

	slog.Debug("Opening browser", "url", oauthURL)
	err = openBrowser(oauthURL) // Use an external library to open a browser
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
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), callbackServerShutdownTimeout)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Debug("Error during OAuth server graceful shutdown", "err", err)
		// Force close if a graceful shutdown fails
		_ = server.Close()
	}

	wg.Wait() // Wait for the server goroutine to finish fully
	slog.Debug("OAuth flow cleanup complete.")
}
