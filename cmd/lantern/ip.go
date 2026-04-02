package main

import (
	"context"
	"fmt"
	"time"

	"github.com/getlantern/publicip"
)

var (
	// list of extra public IP services to query in addition to the default ones provided by the publicip package
	ipURLs = []string{
		"https://ip.me",
		"https://ifconfig.me/ip",
		"https://checkip.amazonaws.com",
		"https://ifconfig.io/ip",
		"https://ident.me",
		"https://ipinfo.io/ip",
	}

	publicIPCfg = &publicip.Config{
		Timeout:      5 * time.Second,
		MinConsensus: 2,
		Methods:      publicip.DefaultMethods(),
	}
)

func init() {
	for _, url := range ipURLs {
		publicIPCfg.Methods = append(publicIPCfg.Methods, publicip.NewHTTP(url, publicip.FormatPlainText))
	}
}

type IPCmd struct{}

func runIP(ctx context.Context) error {
	tctx, tcancel := context.WithTimeout(ctx, 10*time.Second)
	defer tcancel()
	ip, err := getPublicIP(tctx)
	if err != nil {
		return err
	}
	fmt.Println(ip)
	return nil
}

// getPublicIP fetches the public IP address
func getPublicIP(ctx context.Context) (string, error) {
	result, err := publicip.Detect(ctx, publicIPCfg)
	if err != nil {
		return "", err
	}
	if result.IP.IsPrivate() || result.IP.IsLoopback() || result.IP.IsUnspecified() {
		return "", fmt.Errorf("detected IP is not a valid public IP: %s", result.IP.String())
	}
	return result.IP.String(), nil
}
