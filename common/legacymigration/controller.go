package legacymigration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path"
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
	ownerSID    string
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
	c := &Controller{store: store, record: r, verify: verify, adopted: make(chan struct{}), serveErrors: make(chan error, 1), ownerSID: r.SourceSID}
	if r.Status != "pending" {
		close(c.adopted)
	}
	return c, nil
}

func validateRecord(r *Record) error {
	if r == nil || r.Version != 1 || !migrationIDPattern.MatchString(r.MigrationID) || r.SourceSID == "" || r.SourceDirectory == "" {
		return errors.New("invalid migration enrollment")
	}
	if r.Status == "pending" && r.Request == nil && r.Receipt == nil && r.RequestSHA256 == "" && r.TokenSHA256 == "" && len(r.UserData) == 0 {
		return nil
	}
	if (r.Status != "adopted" && r.Status != "completed") || r.Receipt == nil {
		return errors.New("invalid migration record")
	}
	receipt := r.Receipt
	if !validDigest(r.RequestSHA256) || receipt.Version != 1 || receipt.Status != "adopted" || receipt.RequestSHA256 != r.RequestSHA256 || receipt.MigrationID != r.MigrationID || receipt.SourceSID != r.SourceSID || receipt.UserID <= 0 || !validLevel(receipt.UserLevel) || !validText(receipt.DeviceID, 256, true) {
		return errors.New("invalid migration binding")
	}
	if r.Status == "completed" && r.Request == nil && len(r.UserData) == 0 && validDigest(r.TokenSHA256) {
		return nil
	}
	if r.Request == nil || !json.Valid(r.UserData) || r.TokenSHA256 != "" {
		return errors.New("invalid migration record")
	}
	if err := validateRequest(*r.Request, r); err != nil {
		return err
	}
	if receipt.UserID != r.Request.UserID || receipt.DeviceID != r.Request.DeviceID {
		return errors.New("invalid migration binding")
	}
	return nil
}

func validDigest(value string) bool {
	hash, err := hex.DecodeString(value)
	return err == nil && len(hash) == sha256.Size && strings.ToLower(value) == value
}

func digest(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

func validLevel(level string) bool {
	return level == "free" || level == "pro"
}

func validText(value string, max int, required bool) bool {
	return len(value) <= max && (!required || value != "") && strings.IndexFunc(value, unicode.IsControl) < 0
}

func sameDirectory(a, b string) bool {
	return strings.EqualFold(path.Clean(strings.ReplaceAll(a, `\`, "/")), path.Clean(strings.ReplaceAll(b, `\`, "/")))
}

func compatibleEnrollmentUpdate(existing, next *Record) bool {
	if validateRecord(existing) != nil || validateRecord(next) != nil || existing.Version != next.Version || existing.MigrationID != next.MigrationID || existing.SourceSID != next.SourceSID || !sameDirectory(existing.SourceDirectory, next.SourceDirectory) {
		return false
	}
	if existing.Status == "pending" {
		return next.Status != "completed"
	}
	if existing.RequestSHA256 != next.RequestSHA256 || existing.Status == "completed" && next.Status != "completed" {
		return false
	}
	if existing.Status == "adopted" && next.Status == "adopted" {
		receipt := *existing.Receipt
		receipt.UserLevel = next.Receipt.UserLevel
		return *existing.Request == *next.Request && receipt == *next.Receipt
	}
	if *existing.Receipt != *next.Receipt {
		return false
	}
	if next.Request == nil {
		if existing.Request == nil {
			return existing.TokenSHA256 == next.TokenSHA256
		}
		return next.TokenSHA256 == digest([]byte(existing.Request.Token))
	}
	return existing.Request != nil && *existing.Request == *next.Request && bytes.Equal(existing.UserData, next.UserData)
}

func validateRequest(r Request, enrollment *Record) error {
	if r.Version != 1 || r.MigrationID != enrollment.MigrationID || !sameDirectory(r.SourceDirectory, enrollment.SourceDirectory) || r.UserID <= 0 {
		return errors.New("invalid migration request")
	}
	for _, field := range []struct {
		value string
		max   int
		req   bool
	}{{r.Token, 4096, true}, {r.DeviceID, 256, true}, {r.Locale, 128, false}} {
		if !validText(field.value, field.max, field.req) {
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
	hash := digest(raw)
	if c.record.Receipt != nil {
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
	if err != nil || !json.Valid(userData) || !validLevel(level) {
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

// OwnerSID returns the immutable enrolled Windows user SID.
func (c *Controller) OwnerSID() string { return c.ownerSID }

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

// MarkReady durably completes adoption and removes the enrollment's credential copy.
func (c *Controller) MarkReady(identity func() State) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.record.Status != "completed" && !c.matchesState(identity()) {
		return ErrConflict
	}
	if c.record.Status == "completed" && c.record.Request == nil {
		c.identity = identity
		return nil
	}
	next := *c.record
	next.Status = "completed"
	next.TokenSHA256 = digest([]byte(next.Request.Token))
	next.Request, next.UserData = nil, nil
	if err := c.store.Save(&next); err != nil {
		return errPersistence
	}
	c.record, c.identity = &next, identity
	return nil
}

func (c *Controller) matchesState(state State) bool {
	receipt := c.record.Receipt
	if receipt == nil || !state.Healthy || !validLevel(state.UserLevel) || state.UserID != receipt.UserID || state.DeviceID != receipt.DeviceID {
		return false
	}
	if c.record.Request == nil {
		return c.record.TokenSHA256 == digest([]byte(state.Token))
	}
	r := c.record.Request
	return state.Token == r.Token && (c.record.Status == "completed" || (r.Locale == "" || state.Locale == r.Locale) && state.AutoReport == r.AutoReport && state.ProxyAll == r.ProxyAll && state.AutoLaunch == r.AutoLaunch)
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
	if c.record.Receipt == nil || migrationID != c.record.MigrationID || requestSHA256 != c.record.RequestSHA256 {
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
