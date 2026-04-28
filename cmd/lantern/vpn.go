package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/getlantern/radiance/ipc"
	"github.com/getlantern/radiance/vpn"
)

type ConnectCmd struct {
	Name string `arg:"-n,--name" default:"auto" help:"server name to connect to"`
	Wait bool   `arg:"-w,--wait" default:"false" help:"wait for IP change after connecting"`
}

type DisconnectCmd struct{}

type StatusCmd struct{}

func vpnConnect(ctx context.Context, c *ipc.Client, tag string, wait bool) error {
	tctx, tcancel := context.WithTimeout(ctx, 5*time.Second)
	var prevIP string
	if wait {
		prevIP, _ = getPublicIP(tctx)
	}
	tcancel()

	status, err := c.VPNStatus(ctx)
	if err != nil {
		return err
	}
	switch status {
	case vpn.Connected:
		if err := c.SelectServer(ctx, tag); err != nil {
			return err
		}
	case vpn.Disconnected:
		if err := c.ConnectVPN(ctx, tag); err != nil {
			return err
		}
	default:
		return fmt.Errorf("busy with VPN status: %s", status)
	}

	fmt.Printf("Connected (tag: %s)\n", tag)
	if !wait {
		return nil
	}

	fmt.Print("Waiting for IP change...")
	waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
	defer waitCancel()
	start := time.Now()
	ip, err := waitForIPChange(waitCtx, prevIP, 100*time.Millisecond)
	if err == nil && ip != "" {
		fmt.Printf("\rPublic IP: %s (took %v)\n", ip, time.Since(start).Truncate(time.Millisecond))
	} else {
		fmt.Printf("\rIP change not detected after %v\n", time.Since(start).Truncate(time.Second))
	}
	return nil
}

func waitForIPChange(ctx context.Context, current string, interval time.Duration) (string, error) {
	for {
		select {
		case <-ctx.Done():
			return "", nil
		case <-time.After(interval):
			ip, err := getPublicIP(ctx)
			if err != nil {
				return "", nil
			}
			if ip != current {
				return ip, nil
			}
		}
	}
}

func vpnStatus(ctx context.Context, c *ipc.Client) error {
	status, err := c.VPNStatus(ctx)
	if err != nil {
		return err
	}
	line := string(status)
	line = strings.ToUpper(line[:1]) + line[1:] // capitalize first letter
	if status == vpn.Connected {
		if sel, exists, err := c.SelectedServer(ctx); err == nil && exists {
			line += "\nServer: " + sel.Tag
		} else {
			fmt.Printf("error getting selected server: err=%v, sel=%v, exists=%v\n", err, sel, exists)
		}
	}
	tctx, tcancel := context.WithTimeout(ctx, 5*time.Second)
	if ip, err := getPublicIP(tctx); err == nil {
		line += "\nIP: " + ip
	}
	tcancel()
	fmt.Println(line)
	return nil
}
