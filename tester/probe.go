package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/radiance/common/fileperm"
)

// fetchStats is the part of curl's --write-out %{json} the fetch span reports.
type fetchStats struct {
	Status        int     `json:"http_code"`
	ExitCode      *int    `json:"exitcode"`
	Error         string  `json:"errormsg"`
	TimeTotal     float64 `json:"time_total"`
	TimeFirstByte float64 `json:"time_starttransfer"`
	RemoteIP      string  `json:"remote_ip"`
}

// fetch loads target with curl, through the SOCKS proxy when one is given,
// and leaves the response body and curl's metadata in dir.
func fetch(ctx context.Context, target, proxy, dir string, timeout time.Duration) (err error) {
	ctx, span := trace.SpanFromContext(ctx).TracerProvider().Tracer(tracerName).Start(ctx, "tester.fetch",
		trace.WithAttributes(attribute.String("url", target), attribute.Bool("proxied", proxy != "")))
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
		}
		span.End()
	}()
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return errors.New("TARGET_URL must be an HTTPS URL")
	}
	args := []string{"--silent", "--show-error", "--location", "--max-redirs", "10",
		"--proto", "=https", "--proto-redir", "=https", "--max-time", strconv.FormatFloat(timeout.Seconds(), 'f', 3, 64),
		"--output", filepath.Join(dir, "output.txt"), "--write-out", "%{json}"}
	if proxy != "" {
		args = append(args, "--proxy", "socks5h://"+proxy, "--noproxy", "")
	} else {
		args = append(args, "--proxy", "", "--noproxy", "*")
	}
	cmd := exec.CommandContext(ctx, "curl", append(args, target)...)
	cmd.Stderr = os.Stderr
	stats, fetchErr := cmd.Output()
	span.SetAttributes(fetchAttributes(stats)...)
	if err := os.WriteFile(filepath.Join(dir, "fetch.json"), stats, fileperm.Private); err != nil {
		return fmt.Errorf("write fetch metadata: %w", err)
	}
	if fetchErr != nil {
		return fmt.Errorf("fetch: %w", fetchErr)
	}
	return os.WriteFile(filepath.Join(dir, "success"), nil, fileperm.Private)
}

// fetchAttributes reads curl's metadata into span attributes; unreadable
// metadata yields none.
func fetchAttributes(data []byte) []attribute.KeyValue {
	var stats fetchStats
	if json.Unmarshal(data, &stats) != nil || stats.ExitCode == nil {
		return nil
	}
	attrs := []attribute.KeyValue{
		attribute.Int("http.status", stats.Status),
		attribute.Int("curl.exit_code", *stats.ExitCode),
		attribute.Int("total_ms", int(stats.TimeTotal*1000)),
		attribute.Int("first_byte_ms", int(stats.TimeFirstByte*1000)),
	}
	if stats.Error != "" {
		attrs = append(attrs, attribute.String("curl.error", stats.Error))
	}
	if stats.RemoteIP != "" {
		attrs = append(attrs, attribute.String("remote_ip", stats.RemoteIP))
	}
	return attrs
}
