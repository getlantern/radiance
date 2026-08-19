package usermessage

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"time"

	wire "github.com/getlantern/common/usermessage"
)

const (
	initialFailureBackoff = 5 * time.Second
	maxFailureBackoff     = 5 * time.Minute
)

// Clock creates timers and reports the current time.
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

// Timer is the subset of time.Timer used by Service.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time                 { return time.Now() }
func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.Timer.C }

// Options configures a Service.
type Options struct {
	DataDir         string
	Fetcher         Fetcher
	ContextProvider func() ClientContext
	Clock           Clock
	Jitter          func(time.Duration) time.Duration
}

// Service owns polling, per-account presentation state, and display acknowledgment.
type Service struct {
	fetcher         Fetcher
	contextProvider func() ClientContext
	clock           Clock
	jitter          func(time.Duration) time.Duration
	store           *store
	wake            chan struct{}

	mu            sync.Mutex
	started       bool
	active        bool
	online        bool
	generation    uint64
	requestID     uint64
	requestCancel context.CancelFunc
}

// New creates a user-message service and loads its durable state.
func New(opts Options) (*Service, error) {
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
	clock := opts.Clock
	if clock == nil {
		clock = realClock{}
	}
	jitter := opts.Jitter
	if jitter == nil {
		jitter = defaultJitter
	}
	return &Service{
		fetcher:         opts.Fetcher,
		contextProvider: opts.ContextProvider,
		clock:           clock,
		jitter:          jitter,
		store:           state,
		wake:            make(chan struct{}, 1),
		active:          true,
		online:          true,
	}, nil
}

// Start begins with an immediate fetch and is idempotent.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	go s.run(ctx)
}

// Current returns the pending, unexpired message for the current account.
func (s *Service) Current() (*wire.ResolvedUserMessage, error) {
	clientContext := s.contextProvider()
	if clientContext.UserID == "" {
		return nil, nil
	}
	return s.store.current(clientContext.UserID, s.clock.Now())
}

// Refresh requests an immediate fetch. Concurrent requests are coalesced.
func (s *Service) Refresh() {
	s.mu.Lock()
	s.generation++
	cancel := s.requestCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.signalRefresh()
}

func (s *Service) signalRefresh() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Acknowledge marks a pending message as displayed and refreshes eligibility.
func (s *Service) Acknowledge(displayID string) error {
	clientContext := s.contextProvider()
	if clientContext.UserID == "" {
		return ErrMessageNotPending
	}
	if err := s.store.acknowledge(clientContext.UserID, displayID, s.clock.Now()); err != nil {
		return err
	}
	s.Refresh()
	return nil
}

// SetActivity controls polling while the host app is active and online.
func (s *Service) SetActivity(active, online bool) {
	s.mu.Lock()
	changed := s.active != active || s.online != online
	s.active = active
	s.online = online
	s.mu.Unlock()
	if changed {
		s.Refresh()
	}
}

func (s *Service) run(ctx context.Context) {
	var delay time.Duration
	var failures uint
	for s.wait(ctx, delay) {
		requestContext, requestID, generation, ok := s.beginRequest(ctx)
		if !ok {
			delay = 0
			continue
		}
		clientContext := s.contextProvider()
		seen := s.store.seen(clientContext.UserID)
		response, err := s.fetcher.Fetch(requestContext, clientContext, seen)
		s.endRequest(requestID)
		if errors.Is(err, context.Canceled) {
			s.consumeRefresh()
			delay = 0
			continue
		}
		if err == nil {
			pollOnly := response
			pollOnly.Message = nil
			err = pollOnly.Validate()
			if err == nil && response.Message != nil && response.Message.Validate() != nil {
				response.Message = nil
			}
		}
		if err != nil {
			failures++
			delay = s.jitter(failureBackoff(failures))
			continue
		}
		if s.generationChanged(generation) || s.contextProvider() != clientContext {
			s.consumeRefresh()
			delay = 0
			continue
		}
		if err := s.store.offer(clientContext.UserID, response.Message, s.clock.Now()); err != nil {
			failures++
			delay = s.jitter(failureBackoff(failures))
			continue
		}
		failures = 0
		delay = s.jitter(time.Duration(response.PollIntervalSeconds) * time.Second)
	}
}

func (s *Service) beginRequest(parent context.Context) (context.Context, uint64, uint64, bool) {
	requestContext, cancel := context.WithCancel(parent)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || !s.online {
		cancel()
		return nil, 0, 0, false
	}
	s.requestID++
	s.requestCancel = cancel
	return requestContext, s.requestID, s.generation, true
}

func (s *Service) endRequest(requestID uint64) {
	s.mu.Lock()
	var cancel context.CancelFunc
	if s.requestID == requestID {
		cancel = s.requestCancel
		s.requestCancel = nil
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Service) generationChanged(generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation != generation
}

func (s *Service) consumeRefresh() {
	select {
	case <-s.wake:
	default:
	}
}

func (s *Service) wait(ctx context.Context, delay time.Duration) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		if !s.ready() {
			select {
			case <-ctx.Done():
				return false
			case <-s.wake:
				if s.ready() {
					return true
				}
				continue
			}
		}
		if delay <= 0 {
			return true
		}
		timer := s.clock.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-s.wake:
			timer.Stop()
			if s.ready() {
				return true
			}
			continue
		case <-timer.C():
			return true
		}
	}
}

func (s *Service) ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active && s.online
}

func failureBackoff(failures uint) time.Duration {
	delay := initialFailureBackoff
	for i := uint(1); i < failures && delay < maxFailureBackoff; i++ {
		delay *= 2
		if delay >= maxFailureBackoff {
			return maxFailureBackoff
		}
	}
	return delay
}

func defaultJitter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return 0
	}
	return time.Duration(float64(delay) * (0.9 + rand.Float64()*0.1))
}
