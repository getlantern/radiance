package legacymigration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

const maxRequestBytes = 64 << 10

var migrationIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Verify returns authenticated account.UserData JSON and a "free" or "pro" level
// without changing the running account.
type Verify func(context.Context, Request) (json.RawMessage, string, error)

// Controller serializes adoption and exposes readiness only after backend initialization.
// Callbacks run with the controller locked and must not call its methods.
type Controller struct {
	mu          sync.Mutex
	store       Store
	record      *Record
	verify      Verify
	adopted     chan struct{}
	identity    func() State
	serveErrors chan error
	rollback    func(context.Context) error
}

// NewController loads an existing machine enrollment.
func NewController(store Store, verify Verify) (*Controller, error) {
	r, err := store.Load()
	if err != nil {
		return nil, err
	}
	if err := validateRecord(r); err != nil {
		return nil, err
	}
	c := &Controller{store: store, record: r, verify: verify, adopted: make(chan struct{}), serveErrors: make(chan error, 1)}
	if r.Request != nil {
		close(c.adopted)
	}
	return c, nil
}

func validateRecord(r *Record) error {
	if r == nil || r.Version != 1 || !migrationIDPattern.MatchString(r.MigrationID) || r.SourceSID == "" || r.SourceDirectory == "" {
		return errors.New("invalid migration enrollment")
	}
	if r.Status == "pending" && r.Request == nil && r.Receipt == nil && r.RequestSHA256 == "" && len(r.UserData) == 0 {
		return nil
	}
	if (r.Status != "adopted" && r.Status != "completed") || r.Request == nil || r.Receipt == nil || !json.Valid(r.UserData) {
		return errors.New("invalid migration record")
	}
	if err := validateRequest(*r.Request, r); err != nil {
		return err
	}
	hash, err := hex.DecodeString(r.RequestSHA256)
	if err != nil || len(hash) != sha256.Size || r.Receipt.RequestSHA256 != r.RequestSHA256 || r.Receipt.MigrationID != r.MigrationID || r.Receipt.SourceSID != r.SourceSID || r.Receipt.UserID != r.Request.UserID || r.Receipt.DeviceID != r.Request.DeviceID {
		return errors.New("invalid migration binding")
	}
	return nil
}

func validateRequest(r Request, enrollment *Record) error {
	if r.Version != 1 || r.MigrationID != enrollment.MigrationID || !strings.EqualFold(r.SourceDirectory, enrollment.SourceDirectory) || r.UserID <= 0 {
		return errors.New("invalid migration request")
	}
	for _, field := range []struct {
		value string
		max   int
		req   bool
	}{{r.Token, 4096, true}, {r.DeviceID, 256, true}, {r.Locale, 128, false}} {
		if len(field.value) > field.max || (field.req && field.value == "") || strings.IndexFunc(field.value, unicode.IsControl) >= 0 {
			return errors.New("invalid migration request")
		}
	}
	return nil
}

func decodeRequest(raw []byte) (Request, error) {
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, errors.New("invalid migration request")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return request, errors.New("invalid migration request")
	}
	return request, nil
}

// Adopt accepts only the enrolled SID and replays only byte-identical requests.
func (c *Controller) Adopt(ctx context.Context, sid string, raw []byte) (Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sid == "" || sid != c.record.SourceSID {
		return Receipt{}, ErrUnauthorized
	}
	if len(raw) == 0 || len(raw) > maxRequestBytes {
		return Receipt{}, errors.New("invalid migration request")
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(raw))
	if c.record.Request != nil {
		if c.record.RequestSHA256 != hash {
			return Receipt{}, ErrConflict
		}
		return c.receiptLocked(), nil
	}
	request, err := decodeRequest(raw)
	if err != nil {
		return Receipt{}, err
	}
	if err := validateRequest(request, c.record); err != nil {
		return Receipt{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	userData, level, err := c.verify(ctx, request)
	if err != nil || !json.Valid(userData) || (level != "free" && level != "pro") {
		return Receipt{}, errVerification
	}
	receipt := Receipt{Version: 1, MigrationID: request.MigrationID, RequestSHA256: hash,
		UserID: request.UserID, DeviceID: request.DeviceID, UserLevel: level,
		SourceSID: sid, Status: "adopted"}
	next := *c.record
	next.Status, next.RequestSHA256 = "adopted", hash
	next.Request, next.Receipt, next.UserData = &request, &receipt, userData
	if err := c.store.Save(&next); err != nil {
		return Receipt{}, errPersistence
	}
	c.record = &next
	close(c.adopted)
	return receipt, nil
}

var (
	errVerification = errors.New("account verification unavailable")
	errPersistence  = errors.New("migration persistence unavailable")
)

func (c *Controller) receiptLocked() Receipt {
	r := Receipt{Version: 1, MigrationID: c.record.MigrationID, SourceSID: c.record.SourceSID, Status: "pending"}
	if c.record.Receipt != nil {
		r = *c.record.Receipt
		r.Status = "adopted"
	}
	if c.identity != nil && c.matchesState(c.identity()) {
		r.Status = "ready"
	}
	return r
}

// Status returns a credential-free receipt to the enrolled SID.
func (c *Controller) Status(sid string) (Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sid == "" || sid != c.record.SourceSID {
		return Receipt{}, ErrUnauthorized
	}
	return c.receiptLocked(), nil
}

// Completed reports whether the initial adoption reached a healthy backend.
func (c *Controller) Completed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.record.Status == "completed"
}

// Wait blocks until adoption is durable, ctx is canceled, or the listener fails.
// The returned record shares controller data and must not be modified.
func (c *Controller) Wait(ctx context.Context) (*Record, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-c.serveErrors:
		return nil, err
	case <-c.adopted:
		c.mu.Lock()
		defer c.mu.Unlock()
		copy := *c.record
		return &copy, nil
	}
}

// MarkReady durably completes adoption and checks the live identity on subsequent status requests.
func (c *Controller) MarkReady(identity func() State) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.matchesState(identity()) {
		return ErrConflict
	}
	next := *c.record
	next.Status = "completed"
	if err := c.store.Save(&next); err != nil {
		return errPersistence
	}
	c.record, c.identity = &next, identity
	return nil
}

func (c *Controller) matchesState(state State) bool {
	r := c.record.Request
	return r != nil && c.record.Receipt != nil && state.Healthy && state.UserID == r.UserID &&
		state.Token == r.Token && state.DeviceID == r.DeviceID && state.Locale == r.Locale &&
		state.UserLevel == c.record.Receipt.UserLevel && state.AutoReport == r.AutoReport &&
		state.ProxyAll == r.ProxyAll && state.AutoLaunch == r.AutoLaunch
}

// SetRollback installs the operation that disconnects the destination VPN and confirms completion.
func (c *Controller) SetRollback(disconnect func(context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollback = disconnect
}

// Rollback disconnects the destination VPN without changing account or startup state.
func (c *Controller) Rollback(ctx context.Context, sid, migrationID, requestSHA256 string) (Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sid == "" || sid != c.record.SourceSID {
		return Receipt{}, ErrUnauthorized
	}
	if c.record.Request == nil || migrationID != c.record.MigrationID || requestSHA256 != c.record.RequestSHA256 {
		return Receipt{}, ErrConflict
	}
	if c.rollback == nil {
		return Receipt{}, errVerification
	}
	if err := c.rollback(ctx); err != nil {
		return Receipt{}, errVerification
	}
	return c.receiptLocked(), nil
}

type connKey struct{}

// Start serves migration requests asynchronously until ctx is canceled or the returned stop function is called.
func (c *Controller) Start(ctx context.Context) (func() error, error) {
	listener, err := Listen()
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: c, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 10 * time.Second,
		MaxHeaderBytes: 8192, ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			return context.WithValue(ctx, connKey{}, conn)
		}}
	go func() {
		err := server.Serve(listener)
		if err != nil {
			c.serveErrors <- errors.New("migration listener stopped")
		}
	}()
	go func() { <-ctx.Done(); _ = server.Close() }()
	return server.Close, nil
}

// ServeHTTP authenticates each migration request using its local pipe connection.
func (c *Controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, ok := r.Context().Value(connKey{}).(net.Conn)
	if !ok {
		http.Error(w, "permission denied", http.StatusForbidden)
		return
	}
	sid, err := PeerSID(conn)
	if err != nil {
		http.Error(w, "permission denied", http.StatusForbidden)
		return
	}
	var receipt Receipt
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/status":
		receipt, err = c.Status(sid)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/adopt":
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		var raw []byte
		raw, err = io.ReadAll(r.Body)
		if err == nil {
			receipt, err = c.Adopt(r.Context(), sid, raw)
		}
	case r.Method == http.MethodPost && r.URL.Path == "/v1/rollback":
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		var request struct {
			MigrationID   string `json:"migration_id"`
			RequestSHA256 string `json:"request_sha256"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&request)
		if err == nil && decoder.Decode(new(any)) != io.EOF {
			err = errors.New("invalid rollback request")
		}
		if err == nil {
			receipt, err = c.Rollback(r.Context(), sid, request.MigrationID, request.RequestSHA256)
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		code := http.StatusBadRequest
		switch {
		case errors.Is(err, ErrUnauthorized):
			code = http.StatusForbidden
		case errors.Is(err, ErrConflict):
			code = http.StatusConflict
		case errors.Is(err, errVerification), errors.Is(err, errPersistence):
			code = http.StatusServiceUnavailable
		}
		http.Error(w, "migration request failed", code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(receipt)
}
