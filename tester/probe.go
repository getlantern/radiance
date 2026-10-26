package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

func fetch(ctx context.Context, target, proxy, dir string, timeout time.Duration) error {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return errors.New("TARGET_URL must be an HTTPS URL")
	}
	args := []string{"--silent", "--show-error", "--location", "--max-redirs", "10",
		"--proto", "=https", "--proto-redir", "=https", "--max-time", strconv.FormatFloat(timeout.Seconds(), 'f', 3, 64),
		"--output", filepath.Join(dir, "output.txt"), "--write-out", "%{json}"}
	if proxy != "" {
		args = append(args, "--proxy", "socks5h://"+proxy)
	}
	cmd := exec.CommandContext(ctx, "curl", append(args, target)...)
	cmd.Stderr = os.Stderr
	stats, fetchErr := cmd.Output()
	if err := os.WriteFile(filepath.Join(dir, "fetch.json"), stats, 0o600); err != nil {
		return fmt.Errorf("write fetch metadata: %w", err)
	}
	if fetchErr != nil {
		return fmt.Errorf("fetch: %w", fetchErr)
	}
	return os.WriteFile(filepath.Join(dir, "success"), nil, 0o600)
}
