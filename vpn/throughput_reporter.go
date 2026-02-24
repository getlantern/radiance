package vpn

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"time"

	"github.com/getlantern/radiance/vpn/ipc"
)

const (
	throughputReportInterval = 3 * time.Minute
	throughputReportJitter   = 60 // seconds of random jitter added to each interval
	throughputReportTimeout  = 10 * time.Second
	throughputPaddingMax     = 512 // max random padding bytes appended to request body
)

type throughputReport struct {
	Tag        string `json:"tag"`
	BytesDown  int64  `json:"bytes_down"`
	BytesUp    int64  `json:"bytes_up"`
	DurationMs int64  `json:"duration_ms"`
}

type throughputRequest struct {
	Reports []throughputReport `json:"reports"`
	Padding string             `json:"padding,omitempty"`
}

// ThroughputReporter periodically aggregates per-outbound throughput from
// real traffic and POSTs it to the bandit throughput URL.
type ThroughputReporter struct {
	throughputURL string
	interval      time.Duration
	prevBytes     map[string]int64 // outbound tag -> cumulative downlink bytes at last report
	prevTime      time.Time
}

// NewThroughputReporter creates a reporter that will POST throughput data
// to the given URL at regular intervals.
func NewThroughputReporter(throughputURL string) *ThroughputReporter {
	return &ThroughputReporter{
		throughputURL: throughputURL,
		interval:      throughputReportInterval,
		prevBytes:     make(map[string]int64),
	}
}

// Run starts the throughput reporting loop. It blocks until ctx is canceled.
func (r *ThroughputReporter) Run(ctx context.Context) {
	for {
		jitter := randIntn(throughputReportJitter)
		delay := r.interval + time.Duration(jitter)*time.Second
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
			r.report(ctx)
		}
	}
}

func (r *ThroughputReporter) report(ctx context.Context) {
	conns, err := ipc.GetConnections(ctx)
	if err != nil {
		slog.Debug("throughput reporter: failed to get connections", "error", err)
		return
	}

	now := time.Now()

	// Aggregate bytes per outbound tag (FromOutbound field).
	currentBytes := make(map[string]int64)
	for _, conn := range conns {
		if conn.FromOutbound == "" {
			continue
		}
		currentBytes[conn.FromOutbound] += conn.Downlink
	}

	// On the first report, just record the baseline.
	if r.prevTime.IsZero() {
		r.prevBytes = currentBytes
		r.prevTime = now
		return
	}

	durationMs := now.Sub(r.prevTime).Milliseconds()
	if durationMs <= 0 {
		return
	}

	var reports []throughputReport
	for tag, currDown := range currentBytes {
		prevDown := r.prevBytes[tag]
		delta := currDown - prevDown
		if delta <= 0 {
			continue
		}
		reports = append(reports, throughputReport{
			Tag:        tag,
			BytesDown:  delta,
			DurationMs: durationMs,
		})
	}

	r.prevBytes = currentBytes
	r.prevTime = now

	if len(reports) == 0 {
		return
	}

	body, err := json.Marshal(throughputRequest{
		Reports: reports,
		Padding: randPadding(randIntn(throughputPaddingMax)),
	})
	if err != nil {
		slog.Debug("throughput reporter: failed to marshal request", "error", err)
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, throughputReportTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, r.throughputURL, bytes.NewReader(body))
	if err != nil {
		slog.Debug("throughput reporter: failed to create request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Debug("throughput reporter: failed to send report", "error", err)
		return
	}
	resp.Body.Close()

	slog.Debug("throughput reporter: sent report",
		"reports", len(reports),
		"status", resp.StatusCode,
	)
}

// randIntn returns a cryptographically random int in [0, n).
func randIntn(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// randPadding returns a string of n random alphanumeric characters.
func randPadding(n int) string {
	if n <= 0 {
		return ""
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = alphabet[randIntn(len(alphabet))]
	}
	return string(buf)
}
