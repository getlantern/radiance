package usermessage

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	wire "github.com/getlantern/common/usermessage"

	"github.com/getlantern/radiance/common"
)

const (
	initialFailureBackoff = 5 * time.Second
	maxFailureBackoff     = 5 * time.Minute
)

// failureBackoff paces retries between failed fetches. Used for
// deterministic testing of the poll loop.
type failureBackoff interface {
	WaitOn(ctx context.Context, wake <-chan struct{})
	Reset()
}

var newFailureBackoff = func() failureBackoff {
	return common.NewBackoff(initialFailureBackoff, maxFailureBackoff)
}

// Options configures a Service.
type Options struct {
	DataDir         string
	Fetcher         Fetcher
	ContextProvider func() ClientContext
	Logger          *slog.Logger
}

// Service owns polling, per-account presentation state, and display acknowledgment.
type Service struct {
	fetcher         Fetcher
	contextProvider func() ClientContext
	logger          *slog.Logger
	store           *store

	// fetchMu keeps one fetch in flight at a time so the poll loop and a
	// concurrent refresh do not issue duplicate requests off the same seen list.
	fetchMu sync.Mutex

	mu            sync.Mutex
	started       bool
	parent        context.Context
	pollCtx       context.Context
	cancelPolling context.CancelFunc
	// fetchCancel aborts the in-flight fetch so a refresh re-fetches with fresh
	// context instead of waiting for the current request to return.
	fetchCancel context.CancelFunc
	active      bool
	online      bool
	refresh     chan struct{}
}

// New creates a user-message service and loads its durable state.
func New(opts Options) (*Service, error) {
	if opts.DataDir == "" {
		return nil, errors.New("user-message data directory is required")
	}
	if opts.Fetcher == nil {
		return nil, errors.New("user-message fetcher is required")
	}
	if opts.ContextProvider == nil {
		return nil, errors.New("user-message context provider is required")
	}
	state, err := newStore(opts.DataDir)
	if err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default().With("service", "user_messages")
	}
	return &Service{
		fetcher:         opts.Fetcher,
		contextProvider: opts.ContextProvider,
		logger:          logger,
		store:           state,
		active:          true,
		online:          true,
		refresh:         make(chan struct{}, 1),
	}, nil
}

// Start begins with an immediate fetch and is idempotent. Polling runs only
// while the host app is active and online; see [Service.SetActivity].
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return
	}
	s.started = true
	s.parent = ctx
	s.startPollingLocked()
}

// Current returns the pending, unexpired message for the current account.
func (s *Service) Current() (*wire.ResolvedUserMessage, error) {
	clientContext := s.contextProvider()
	if clientContext.UserID == "" {
		return nil, nil
	}
	return s.store.current(clientContext.UserID, time.Now())
}

func (s *Service) Refresh() {
	s.mu.Lock()
	cancel := s.fetchCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case s.refresh <- struct{}{}:
	default:
	}
}

// Acknowledge marks a pending message as displayed and refreshes eligibility.
func (s *Service) Acknowledge(displayID string) error {
	clientContext := s.contextProvider()
	if clientContext.UserID == "" {
		return ErrMessageNotPending
	}
	if err := s.store.acknowledge(clientContext.UserID, displayID, time.Now()); err != nil {
		return err
	}
	s.Refresh()
	return nil
}

// SetActivity controls polling while the host app is active and online.
// Resuming begins with an immediate fetch; suspending cancels any request in
// flight.
func (s *Service) SetActivity(active, online bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == active && s.online == online {
		return
	}
	s.active = active
	s.online = online
	if active && online {
		s.startPollingLocked()
		return
	}
	s.stopPollingLocked()
}

func (s *Service) startPollingLocked() {
	if !s.started || !s.active || !s.online || s.pollCtx != nil || s.parent.Err() != nil {
		return
	}
	// Drop a refresh queued while stopped; the run below already fetches
	// immediately, so a stale wake would only trigger a second fetch.
	select {
	case <-s.refresh:
	default:
	}
	ctx, cancel := context.WithCancel(s.parent)
	s.pollCtx, s.cancelPolling = ctx, cancel
	go s.run(ctx)
}

func (s *Service) stopPollingLocked() {
	if s.cancelPolling == nil {
		return
	}
	s.cancelPolling()
	s.pollCtx, s.cancelPolling = nil, nil
}

func (s *Service) run(ctx context.Context) {
	var failures uint
	backoff := newFailureBackoff()
	for ctx.Err() == nil {
		delay, err := s.fetchAndStore(ctx)
		// Checked ahead of err so a fetch aborted by a stopped poll context
		// (shutdown or suspend) is not counted as a fetch failure.
		if ctx.Err() != nil {
			break
		}
		if errors.Is(err, context.Canceled) {
			// A refresh aborted the in-flight fetch; drop its wake and re-fetch now.
			select {
			case <-s.refresh:
			default:
			}
			continue
		}
		if err != nil {
			failures++
			s.logFetchFailure(err, failures)
			backoff.WaitOn(ctx, s.refresh)
			continue
		}
		failures = 0
		backoff.Reset()

		select {
		case <-ctx.Done():
		case <-time.After(delay):
		case <-s.refresh:
		}
	}
	s.logger.Debug("User-message polling stopped", "reason", ctx.Err())
}

// fetchAndStore returns the delay the server recommends before the next fetch.
// A zero delay with a nil error means the response was discarded because the
// account changed while it was in flight.
func (s *Service) fetchAndStore(ctx context.Context) (time.Duration, error) {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()

	clientContext := s.contextProvider()
	if !clientContext.valid() {
		return 0, errCredentialsUnavailable
	}
	seen := s.store.seen(clientContext.UserID)

	fetchCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.fetchCancel = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.fetchCancel = nil
		s.mu.Unlock()
		cancel()
	}()

	response, err := s.fetcher.Fetch(fetchCtx, clientContext, seen)
	if err != nil {
		return 0, err
	}
	pollOnly := response
	pollOnly.Message = nil
	if err := pollOnly.Validate(); err != nil {
		return 0, invalidResponseError{err: err}
	}
	if response.Message != nil && response.Message.Validate() != nil {
		s.logger.Warn("User-message response discarded", "category", "invalid_message")
		response.Message = nil
	}
	if s.contextProvider() != clientContext {
		return 0, nil
	}
	delay := time.Duration(response.PollIntervalSeconds) * time.Second
	if err := s.store.offer(clientContext.UserID, response.Message, time.Now()); err != nil {
		s.logger.Warn(
			"User-message fetch result could not be persisted",
			"category", "local_state",
		)
		return delay, nil
	}
	s.logFetchResult(response.Message, delay)
	return delay, nil
}

func (s *Service) logFetchFailure(err error, failures uint) {
	category, statusCode := fetchFailureDetails(err)
	// Do not attach err here. Transport errors can contain request URLs, and
	// future error wrappers might include credentials or localized content.
	attributes := []any{"category", category}
	if failures > 0 {
		attributes = append(attributes, "failure_count", failures)
	}
	if statusCode != 0 {
		attributes = append(attributes, "http_status", statusCode)
	}
	level := slog.LevelWarn
	if errors.Is(err, errCredentialsUnavailable) {
		// A signed-out client retries on the same ladder as a real failure, so
		// this recurs indefinitely and is not actionable.
		level = slog.LevelDebug
	}
	s.logger.Log(context.Background(), level, "User-message fetch failed", attributes...)
}

func fetchFailureDetails(err error) (category string, statusCode int) {
	var invalidResponse invalidResponseError
	if errors.As(err, &invalidResponse) {
		return "invalid_response", 0
	}
	if errors.Is(err, errCredentialsUnavailable) {
		return "credentials_unavailable", 0
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", 0
	}
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) {
		return "transport", 0
	}
	statusCode = statusErr.statusCode
	switch {
	case statusCode == 400:
		category = "request_rejected"
	case statusCode == 401 || statusCode == 403:
		category = "authentication"
	case statusCode == 404:
		category = "endpoint"
	case statusCode == 408:
		category = "timeout"
	case statusCode == 429:
		category = "rate_limited"
	case statusCode >= 500:
		category = "server"
	default:
		category = "http"
	}
	return category, statusCode
}

func (s *Service) logFetchResult(message *wire.ResolvedUserMessage, pollIn time.Duration) {
	if message == nil {
		s.logger.Debug("User-message fetch completed", "result", "no_message", "poll_in", pollIn)
		return
	}
	s.logger.Info(
		"User-message fetch completed",
		"result", "message_available",
		"campaign_id", message.CampaignID,
		"revision_id", message.RevisionID,
		"delivery_id", message.DeliveryID,
		"surface", message.Surface,
		"locale", message.Locale,
		"expires_at", message.ExpiresAt,
		"poll_in", pollIn,
	)
}

type invalidResponseError struct {
	err error
}

func (e invalidResponseError) Error() string {
	return "invalid user-message response"
}

func (e invalidResponseError) Unwrap() error {
	return e.err
}
