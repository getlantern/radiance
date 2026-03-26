package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/getlantern/radiance/ipc"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn"
)

type ServersCmd struct {
	Show           string `arg:"--show" help:"display server by tag"`
	AddJSON        string `arg:"--add-json" help:"add servers from JSON config"`
	AddURL         string `arg:"--add-url" help:"add servers from comma-separated URLs"`
	SkipCertVerify bool   `arg:"--skip-cert-verify" help:"skip cert verification (with --add-url)"`
	Remove         string `arg:"--remove" help:"comma-separated list of servers to remove"`
	List           bool   `arg:"--list" help:"list servers"`

	PrivateServer *PrivateServerCmd `arg:"subcommand:private" help:"private server operations"`
}

type PrivateServerCmd struct {
	Add          string `arg:"--add" help:"add private server with given tag"`
	Invite       string `arg:"--invite" help:"invite to private server"`
	RevokeInvite string `arg:"--revoke-invite" help:"revoke invite"`
	IP           string `arg:"--ip" help:"server IP"`
	Port         int    `arg:"--port" help:"server port"`
	Token        string `arg:"--token" help:"access token"`
}

func runServers(ctx context.Context, c *ipc.Client, cmd *ServersCmd) error {
	switch {
	case cmd.Show != "":
		return serversGet(ctx, c, cmd.Show)
	case cmd.AddJSON != "":
		return c.AddServersByJSON(ctx, cmd.AddJSON)
	case cmd.AddURL != "":
		urls := strings.Split(cmd.AddURL, ",")
		return c.AddServersByURL(ctx, urls, cmd.SkipCertVerify)
	case cmd.Remove != "":
		return serversRemove(ctx, c, cmd.Remove)
	case cmd.List:
		return serversList(ctx, c)
	case cmd.PrivateServer != nil:
		return runPrivateServer(ctx, c, cmd.PrivateServer)
	default:
		return fmt.Errorf("must specify one of --get, --add-json, --add-url, --remove, or --list")
	}
}

func runPrivateServer(ctx context.Context, c *ipc.Client, cmd *PrivateServerCmd) error {
	switch {
	case cmd.Add != "":
		return c.AddPrivateServer(ctx, cmd.Add, cmd.IP, cmd.Port, cmd.Token)
	case cmd.Invite != "":
		code, err := c.InviteToPrivateServer(ctx, cmd.IP, cmd.Port, cmd.Token, cmd.Invite)
		if err != nil {
			return err
		}
		fmt.Println(code)
		return nil
	case cmd.RevokeInvite != "":
		return c.RevokePrivateServerInvite(ctx, cmd.IP, cmd.Port, cmd.Token, cmd.RevokeInvite)
	default:
		return fmt.Errorf("must specify one of --add, --invite, or --revoke-invite")
	}
}

func serversList(ctx context.Context, c *ipc.Client) error {
	srvs, err := c.Servers(ctx)
	if err != nil {
		return err
	}
	found := false
	for group, opts := range srvs {
		if len(opts.Outbounds) == 0 && len(opts.Endpoints) == 0 {
			continue
		}
		found = true
		fmt.Println(group)
		for _, s := range opts.Outbounds {
			printServerEntry(s.Tag, s.Type, opts)
		}
		for _, s := range opts.Endpoints {
			printServerEntry(s.Tag, s.Type, opts)
		}
	}
	if !found {
		fmt.Println("No servers available")
	}
	return nil
}

func printServerEntry(tag, typ string, opts servers.Options) {
	fmt.Printf("  %s [%s]", tag, typ)
	if loc, ok := opts.Locations[tag]; ok {
		fmt.Printf(" — %s, %s", loc.City, loc.Country)
	}
	fmt.Println()
}

func serversGet(ctx context.Context, c *ipc.Client, tag string) error {
	svr, exists, err := c.GetServerByTag(ctx, tag)
	if err != nil {
		return err
	}
	if !exists {
		fmt.Println("Server not found")
		return nil
	}
	return printJSON(svr)
}

func serversSelected(ctx context.Context, c *ipc.Client) error {
	svr, exists, err := c.SelectedServer(ctx)
	if err != nil {
		return err
	}
	if !exists {
		fmt.Println("No server selected")
		return nil
	}
	return printJSON(svr)
}

func serversAutoSelections(ctx context.Context, c *ipc.Client, watch bool) error {
	if watch {
		return c.AutoSelectedEvents(ctx, func(ev vpn.AutoSelectedEvent) {
			s := ev.Selected
			fmt.Printf("Selected: %s\n", s)
		})
	}
	sel, err := c.AutoSelected(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Selected: %s\n", sel.Tag)
	return nil
}

func serversRemove(ctx context.Context, c *ipc.Client, tags string) error {
	tagList := strings.Split(tags, ",")
	return c.RemoveServers(ctx, tagList)
}
