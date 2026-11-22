package vpn

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gofrs/flock"

	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/fileperm"
	"github.com/getlantern/radiance/helper"
)

// usageCategory is the path a connection's bytes are counted under.
type usageCategory uint32

const (
	usageNone usageCategory = iota
	usageProxyless
	usagePersonal
	usagePrivate
	usageLantern
	usageDirect
	usageCategories
)

// UsageBytes is the bytes sent (Up) and received (Down) along one path.
type UsageBytes struct {
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

// UsageTotals is the bytes along each path: proxyless, personal proxies,
// private (user-added) servers, Lantern's servers, and direct (traffic smart
// routing or split tunneling sends around the tunnel's proxies).
type UsageTotals struct {
	Proxyless UsageBytes `json:"proxyless"`
	Personal  UsageBytes `json:"personal"`
	Private   UsageBytes `json:"private"`
	Lantern   UsageBytes `json:"lantern"`
	Direct    UsageBytes `json:"direct"`
}

func (t *UsageTotals) at(c usageCategory) *UsageBytes {
	switch c {
	case usageProxyless:
		return &t.Proxyless
	case usagePersonal:
		return &t.Personal
	case usagePrivate:
		return &t.Private
	case usageLantern:
		return &t.Lantern
	case usageDirect:
		return &t.Direct
	}
	return nil
}

func (t *UsageTotals) add(o UsageTotals) {
	for c := usageNone + 1; c < usageCategories; c++ {
		a, b := t.at(c), o.at(c)
		a.Up += b.Up
		a.Down += b.Down
	}
}

// Usage is the bytes the tunnel carried along each path since Since, and in
// Month (YYYY-MM, on the device's calendar). It holds byte totals only: no
// destination, server or time of use.
type Usage struct {
	Since     time.Time   `json:"since"`
	Month     string      `json:"month"`
	Lifetime  UsageTotals `json:"lifetime"`
	ThisMonth UsageTotals `json:"this_month"`
	// ResetAt is when the totals were last reset, if ever. It tells a reset
	// from a store's first save, which also starts a file's Since.
	ResetAt time.Time `json:"reset_at,omitzero"`
}

// rollTo moves u to month if month replaces u.Month, starting the month's
// totals from zero. Months are YYYY-MM, so they compare as strings. A later
// month replaces it, and so does an earlier one when u.Month is after next,
// which means it was saved while the clock ran ahead; a clock stepped back
// by less than that keeps counting into the later month.
func (u *Usage) rollTo(month, next string) {
	if u.Month == "" || month > u.Month || u.Month > next {
		u.Month = month
		u.ThisMonth = UsageTotals{}
	}
}

// UsageStats counts the bytes each path carries and keeps the totals in a
// file in the data directory. It outlives tunnels: each tunnel's tracker adds
// to it on the data path, and it saves every Run interval and on Flush, so a
// killed process loses at most one interval. Nothing in it leaves the device.
//
// The app and the VPN extension can each run a backend on the same file, so a
// store keeps in memory only what it counted since its last save, and a save
// adds that to the file under a cross-process lock, as helper.ActivityTracker
// does.
type UsageStats struct {
	path string
	now  func() time.Time
	loc  func() *time.Location

	pending [usageCategories]struct{ up, down atomic.Int64 }
	// pendingFrom is when the pending totals last went from empty to not,
	// in Unix nanoseconds, or 0 while they're empty. A save compares it with
	// a reset another store made, to keep bytes counted after the reset.
	pendingFrom atomic.Int64
	zone        atomic.Pointer[time.Location]

	// writeMu serializes this store's saves and resets, across the wait for
	// the file lock. saveMu is held across a save's read, take and write of
	// pending, and by Usage, so a reader sees bytes either pending or in the
	// file, never both or neither.
	writeMu sync.Mutex
	saveMu  sync.Mutex
	// since is the file's Since when this store last read or wrote it,
	// guarded by saveMu. Since changes only on a reset, so a file whose
	// Since differs was reset by another store.
	since time.Time

	warnLockOnce sync.Once

	runMu   sync.Mutex
	stopRun context.CancelFunc
	runDone chan struct{}
	stopped bool
}

// NewUsageStats returns a store that keeps its totals at path. It reads the
// file's Since now, before it counts anything, so a reset another store makes
// before this one's first save is seen as one.
func NewUsageStats(path string) *UsageStats {
	// A store that saved them in another mode, such as the root network
	// extension on macOS, left them unreadable to the app.
	shareUsageFile(path, false)
	shareUsageFile(path+".lock", false)
	return &UsageStats{path: path, now: time.Now, loc: helper.LocalZone, since: readUsage(path).Since}
}

// count adds n bytes, sent if up, to c's pending total. It runs on the data
// path. usageNone's slot is never read.
func (s *UsageStats) count(c usageCategory, up bool, n int64) {
	if up {
		s.pending[c].up.Add(n)
	} else {
		s.pending[c].down.Add(n)
	}
	// Only the first chunk after a save reads the clock.
	if s.pendingFrom.Load() == 0 {
		s.pendingFrom.CompareAndSwap(0, s.now().UnixNano())
	}
}

// pendingBeforeLocked reports whether the pending totals started before
// reset, so they belong to the totals that reset cleared. Bytes counted
// between the reset and the first chunk after it are dropped with them, at
// most one save interval's worth. Caller holds saveMu.
func (s *UsageStats) pendingBeforeLocked(reset time.Time) bool {
	from := s.pendingFrom.Load()
	return from != 0 && from <= reset.UnixNano()
}

// idle reports whether nothing is pending and the store already saw the file,
// so a save has nothing to do.
//
// A chunk counted while take drains can leave pendingFrom set with its bytes
// already taken. With nothing pending, idle clears it, so bytes counted after
// a later reset don't inherit it and get dropped. A chunk counted between the
// check and the clear keeps its bytes and loses only its start time, so it
// counts as after any reset: at most one interval's worth, like the other
// reset bounds.
func (s *UsageStats) idle() bool {
	s.saveMu.Lock()
	seen := !s.since.IsZero()
	s.saveMu.Unlock()
	if s.peek() != (UsageTotals{}) {
		return false
	}
	if from := s.pendingFrom.Load(); from != 0 {
		s.pendingFrom.CompareAndSwap(from, 0)
	}
	return seen
}

func (s *UsageStats) peek() UsageTotals {
	var t UsageTotals
	for c := usageNone + 1; c < usageCategories; c++ {
		b := t.at(c)
		b.Up, b.Down = s.pending[c].up.Load(), s.pending[c].down.Load()
	}
	return t
}

// take empties the pending totals and returns them with when they started.
// pendingFrom is cleared first, so a chunk counted meanwhile restarts it no
// later than the chunk itself.
func (s *UsageStats) take() (UsageTotals, int64) {
	from := s.pendingFrom.Swap(0)
	var t UsageTotals
	for c := usageNone + 1; c < usageCategories; c++ {
		b := t.at(c)
		b.Up, b.Down = s.pending[c].up.Swap(0), s.pending[c].down.Swap(0)
	}
	return t, from
}

// giveBack returns totals that take took, keeping the earlier start.
func (s *UsageStats) giveBack(t UsageTotals, from int64) {
	for c := usageNone + 1; c < usageCategories; c++ {
		b := t.at(c)
		s.pending[c].up.Add(b.Up)
		s.pending[c].down.Add(b.Down)
	}
	for from != 0 {
		cur := s.pendingFrom.Load()
		if cur != 0 && cur <= from || s.pendingFrom.CompareAndSwap(cur, from) {
			return
		}
	}
}

// months returns this month and next in the device's zone. Resolving the
// zone can walk the zone database, so readers use the one the last save
// resolved.
func (s *UsageStats) months(refresh bool) (month, next string) {
	zone := s.zone.Load()
	if zone == nil || refresh {
		zone = s.loc()
		s.zone.Store(zone)
	}
	now := s.now().In(zone)
	y, m, _ := now.Date()
	return now.Format("2006-01"), time.Date(y, m+1, 1, 0, 0, 0, 0, zone).Format("2006-01")
}

func readUsage(path string) Usage {
	u, _ := readUsageChecked(path)
	return u
}

// readUsageChecked reads the file, reporting false when it exists but can't
// be read: a save must not then write totals rebuilt from nothing over it. A
// file that reads but doesn't parse is lost already, and is replaced.
func readUsageChecked(path string) (Usage, bool) {
	var u Usage
	data, err := os.ReadFile(path)
	if err != nil {
		return u, errors.Is(err, fs.ErrNotExist)
	}
	_ = json.Unmarshal(data, &u)
	return u, true
}

// usageReadWait bounds Usage's wait for the store, and usageResetWait Reset's
// for this store's save, which can spend usageLockWait on the file lock and
// then do its I/O. Neither bounds a request's own I/O.
var (
	usageReadWait  = 250 * time.Millisecond
	usageResetWait = 3 * time.Second
)

// lockWithin locks mu, giving up after d. It polls while mu is held.
func lockWithin(mu *sync.Mutex, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for !mu.TryLock() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// Usage returns the totals: the file's, which every store on it has added to,
// plus this store's pending ones. It resolves the device's zone afresh, since
// an idle store's saves don't, and the zone decides the month. It returns
// ErrUsageBusy if a save or reset holds the store past usageReadWait.
func (s *UsageStats) Usage() (Usage, error) {
	month, next := s.months(true)
	if !lockWithin(&s.saveMu, usageReadWait) {
		return Usage{}, ErrUsageBusy
	}
	defer s.saveMu.Unlock()
	return s.usageLocked(month, next)
}

func (s *UsageStats) usageLocked(month, next string) (Usage, error) {
	u, ok := readUsageChecked(s.path)
	if !ok {
		return Usage{}, errUsageRead
	}
	stale := s.resetElsewhereLocked(u) && s.pendingBeforeLocked(u.Since)
	if u.Since.IsZero() {
		u.Since = s.now()
	}
	u.rollTo(month, next)
	if !stale {
		pending := s.peek()
		u.Lifetime.add(pending)
		u.ThisMonth.add(pending)
	}
	return u, nil
}

// resetElsewhereLocked reports whether another store reset file since this
// one last saw it. Caller holds saveMu.
func (s *UsageStats) resetElsewhereLocked(file Usage) bool {
	if s.since.IsZero() {
		// No baseline: the file was missing when this store started, so
		// only a reset's own mark says the file was reset.
		return !file.ResetAt.IsZero()
	}
	return !file.Since.IsZero() && !file.Since.Equal(s.since)
}

// usageLockWait bounds how long a save or reset waits for the cross-process
// lock; see activityLockWait in package helper.
var usageLockWait = 500 * time.Millisecond

// lock takes the cross-process file lock. It returns errUsageLock if the lock
// file can't be created or opened for good (no permission, a read-only or
// missing directory, a path through a file or onto a directory), and
// ErrUsageBusy otherwise: for a lock another process
// held past usageLockWait, or an error that may clear, such as running out of
// file descriptors.
func (s *UsageStats) lock() (*flock.Flock, error) {
	// fileperm.File, like the totals: the app and the root network extension
	// on macOS both take the lock. Creating it here sets the mode whatever the
	// umask, which flock's own create doesn't.
	shareUsageFile(s.path+".lock", true)
	l := flock.New(s.path+".lock", flock.SetPermissions(fileperm.File))
	ctx, cancel := context.WithTimeout(context.Background(), usageLockWait)
	defer cancel()
	locked, err := l.TryLockContext(ctx, 25*time.Millisecond)
	switch {
	case err == nil && locked:
		return l, nil
	case errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, syscall.EROFS) || errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.ENOTDIR):
		return nil, errUsageLock
	}
	return nil, ErrUsageBusy
}

// write replaces the file. Errors are fixed text: the path can name the
// device's user.
func (s *UsageStats) write(u Usage) bool {
	data, err := json.Marshal(u)
	if err != nil {
		return false
	}
	// fileperm.File, not Private: on macOS the app and the root network
	// extension both read it from the shared data dir, the app while the VPN
	// is off.
	if atomicfile.WriteFile(s.path, data, fileperm.File) != nil {
		slog.Debug("vpn: saving usage stats failed")
		return false
	}
	return true
}

// Flush adds the pending totals to the file, rolling its month forward. It
// writes nothing when the file would not change. Without the file lock, or if
// the write fails, the totals stay pending for the next save.
//
// With nothing pending, once the store has seen the file, it returns without
// touching the disk: Run calls it every interval, and an idle device should
// pay no more than that check. A month that ended meanwhile rolls on the next
// read or save.
func (s *UsageStats) Flush() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.idle() {
		return
	}
	l, err := s.lock()
	if errors.Is(err, errUsageLock) {
		s.warnLockOnce.Do(func() { slog.Warn("vpn: usage stats lock file unusable; not saving") })
		return
	}
	if err != nil {
		slog.Debug("vpn: usage stats file lock busy; saving later")
		return
	}
	defer func() { _ = l.Unlock() }()

	month, next := s.months(true)
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	file, ok := readUsageChecked(s.path)
	if !ok {
		slog.Debug("vpn: reading usage stats failed; saving later")
		return
	}
	if s.resetElsewhereLocked(file) && s.pendingBeforeLocked(file.Since) {
		s.take()
	}
	s.since = file.Since
	u := file
	if u.Since.IsZero() {
		u.Since = s.now()
	}
	u.rollTo(month, next)
	pending, from := s.take()
	u.Lifetime.add(pending)
	u.ThisMonth.add(pending)
	if u == file {
		return
	}
	if !s.write(u) {
		s.giveBack(pending, from)
		return
	}
	s.since = u.Since
}

// Reset clears the totals, here and in the file, and counts from now.
func (s *UsageStats) Reset() error {
	if !lockWithin(&s.writeMu, usageResetWait) {
		return ErrUsageBusy
	}
	defer s.writeMu.Unlock()
	l, err := s.lock()
	if err != nil {
		return err
	}
	defer func() { _ = l.Unlock() }()

	month, _ := s.months(true)
	if !lockWithin(&s.saveMu, usageReadWait) {
		return ErrUsageBusy
	}
	defer s.saveMu.Unlock()
	pending, from := s.take()
	since := s.now()
	if !s.write(Usage{Since: since, Month: month, ResetAt: since}) {
		s.giveBack(pending, from)
		return errUsageWrite
	}
	s.since = since
	return nil
}

// ErrUsageBusy means another save or reset, in this process or another, or a
// read in this one, held the stats past the request's wait; the request can be
// retried.
var ErrUsageBusy = errors.New("usage stats are busy")

var (
	// errUsageLock means the lock file can't be used; a retry won't help.
	errUsageLock  = errors.New("could not lock usage stats")
	errUsageRead  = errors.New("could not read usage stats")
	errUsageWrite = errors.New("could not save usage stats")
)

// Run saves every interval until ctx ends or Stop is called. It returns at
// once if Stop was called, or if another Run hasn't returned yet: one runs at
// a time, so Stop joins the only one.
func (s *UsageStats) Run(ctx context.Context, every time.Duration) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	s.runMu.Lock()
	if s.stopped || s.stopRun != nil {
		s.runMu.Unlock()
		return
	}
	s.stopRun, s.runDone = cancel, done
	s.runMu.Unlock()
	defer func() {
		s.runMu.Lock()
		if s.runDone == done {
			s.stopRun, s.runDone = nil, nil
		}
		s.runMu.Unlock()
	}()
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			s.Flush()
		}
	}
}

// Stop ends Run and waits for it. A Run that starts afterwards returns at
// once.
func (s *UsageStats) Stop() {
	s.runMu.Lock()
	s.stopped = true
	stop, done := s.stopRun, s.runDone
	s.runMu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}

// usageSink is what a tunnel's tracker needs to count bytes by path.
type usageSink struct {
	stats     *UsageStats
	isPrivate func(tag string) bool
}

// hedgeReports forwards the hedges' reports to the tunnel's tracker once it
// exists. The box looks the observer up as it builds the hedges, before the
// tracker can be made, but no connection is routed until after, so the
// reports dropped meanwhile are none that matter.
type hedgeReports struct {
	tracker atomic.Pointer[connTracker]
}

func (h *hedgeReports) FirstCommitted(ctx context.Context, hedge, first string) {
	if t := h.tracker.Load(); t != nil {
		t.FirstCommitted(ctx, hedge, first)
	}
}
