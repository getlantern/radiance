package vpn

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
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

// getPublicIP fetches the public IP address by querying multiple external services concurrently.
// It returns the first successful response or an error if all requests fail.
func getPublicIP() (string, error) {
	type result struct {
		ip  string
		err error
	}
	results := make(chan result, len(ipURLs))
	sem := make(chan struct{}, 4)

	client := &http.Client{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, url := range ipURLs {
		go func() {
			// limit number of concurrent requests
			sem <- struct{}{}
			defer func() { <-sem }()
			ip, err := fetchIP(ctx, client, url)
			results <- result{ip, err}
		}()
	}

	var lastErr error
	for i := 0; i < len(ipURLs); i++ {
		res := <-results
		if res.err == nil {
			return res.ip, nil
		}
		lastErr = res.err
	}
	return "", lastErr
}

// fetchIP performs an HTTP GET request to the given URL and returns the trimmed response body as the IP.
func fetchIP(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "curl/8.14.1") // some services return the entire HTML page for non-curl user agents
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
		return "", fmt.Errorf("response is not a valid IP: %s -> %s...", url, ip[:7])
	}
	return ip, nil
}
