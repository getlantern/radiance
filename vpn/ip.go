package vpn

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// list of URLs to fetch the public IP address, just in case one is down or blocked
var ipURLs = []string{
	"https://ip.me",
	"https://ifconfig.me/ip",
	"https://checkip.amazonaws.com",
	"https://ifconfig.io/ip",
	"https://ident.me",
	"https://ipinfo.io/ip",
	"https://api.ipify.org",
}

// GetPublicIP fetches the public IP address
func GetPublicIP() (string, error) {
	return getPublicIP(context.Background(), ipURLs)
}

func getPublicIP(ctx context.Context, urls []string) (string, error) {
	if len(urls) == 0 {
		urls = ipURLs
	}
	type result struct {
		ip  string
		err error
	}
	results := make(chan result, len(urls))
	sem := make(chan struct{}, 3)

	client := &http.Client{}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, url := range urls {
		go func() {
			// limit number of concurrent requests
			sem <- struct{}{}
			defer func() { <-sem }()
			ip, err := fetchIP(ctx, client, url)
			results <- result{ip, err}
		}()
	}

	var lastErr error
	for i := 0; i < len(urls); i++ {
		res := <-results
		if res.err == nil {
			return res.ip, nil
		}
		lastErr = res.err
	}
	return "", fmt.Errorf("failed to get public IP, error: %w", lastErr)
}

// fetchIP performs an HTTP GET request to the given URL and returns the trimmed response body as the IP.
func fetchIP(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "curl/8.14.1") // some services return the entire HTML page for non-curl user agents
	req.Header.Set("Connection", "close")
	req.Close = true
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if ip == "" {
		return "", fmt.Errorf("empty response from %s", url)
	}
	if _, err := netip.ParseAddr(ip); err != nil {
		return "", fmt.Errorf("response is not a valid IP: %s -> %s...", url, ip[:min(len(ip), 7)])
	}
	return ip, nil
}

// WaitForIPChange polls the public IP address every interval until it changes from the current value.
func WaitForIPChange(ctx context.Context, current string, interval time.Duration) (string, error) {
	urls := ipURLs
	for {
		select {
		case <-ctx.Done():
			return "", nil
		case <-time.After(interval):
			ip, err := getPublicIP(ctx, urls)
			if err != nil {
				return "", nil
			} else if ip != current {
				return ip, nil
			}
			urls = append(urls[3:], urls[:3]...) // rotate URLs to avoid hitting the same ones repeatedly
		}
	}
}
