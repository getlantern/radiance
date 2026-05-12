package main

import (
	"context"
	"fmt"
	"strings"

	C "github.com/getlantern/common"

	"github.com/getlantern/radiance/ipc"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn"
)

type ServersCmd struct {
	List          *ServersListCmd    `arg:"subcommand:list" help:"list servers"`
	Show          *ServersShowCmd    `arg:"subcommand:show" help:"display server by tag"`
	AddJSON       *ServersAddJSONCmd `arg:"subcommand:add-json" help:"add servers from JSON config"`
	AddURL        *ServersAddURLCmd  `arg:"subcommand:add-url" help:"add servers from URLs"`
	Remove        *ServersRemoveCmd  `arg:"subcommand:remove" help:"remove servers by tag"`
	PrivateServer *PrivateServerCmd  `arg:"subcommand:private" help:"private server operations"`
}

type ServersListCmd struct {
	Latency bool `arg:"--latency" help:"include URL test latency results"`
	JSON    bool `arg:"--json" help:"output JSON"`
}

type ServersShowCmd struct {
	Tag string `arg:"positional,required" help:"server tag"`
}

type ServersAddJSONCmd struct {
	Config string `arg:"positional,required" help:"JSON config"`
}

type ServersAddURLCmd struct {
	URLs           []string `arg:"positional,required" help:"server URLs"`
	SkipCertVerify bool     `arg:"--skip-cert-verify" help:"skip cert verification"`
}

type ServersRemoveCmd struct {
	Tags []string `arg:"positional,required" help:"server tags to remove"`
}

// ServerListEntry represents a server in the list output.
type ServerListEntry struct {
	Tag           string                 `json:"tag"`
	Type          string                 `json:"type"`
	Location      C.ServerLocation       `json:"location,omitempty"`
	URLTestResult *servers.URLTestResult `json:"urlTestResult,omitempty"`
}

type PrivateServerCmd struct {
	Add          *PrivateServerAddCmd          `arg:"subcommand:add" help:"add a private server"`
	Invite       *PrivateServerInviteCmd       `arg:"subcommand:invite" help:"create an invite for a private server"`
	RevokeInvite *PrivateServerRevokeInviteCmd `arg:"subcommand:revoke-invite" help:"revoke a private server invite"`
}

// PrivateServerConn holds connection parameters for a private server.
type PrivateServerConn struct {
	IP    string `arg:"--ip,required" help:"server IP"`
	Port  int    `arg:"--port,required" help:"server port"`
	Token string `arg:"--token,required" help:"access token"`
}

type PrivateServerAddCmd struct {
	Tag string `arg:"positional,required" help:"tag to assign to the server"`
	PrivateServerConn
}

type PrivateServerInviteCmd struct {
	Name string `arg:"positional,required" help:"invitee name"`
	PrivateServerConn
}

type PrivateServerRevokeInviteCmd struct {
	Name string `arg:"positional,required" help:"invitee name to revoke"`
	PrivateServerConn
}

func runServers(ctx context.Context, c *ipc.Client, cmd *ServersCmd) error {
	switch {
	case cmd.Show != nil:
		return serversGet(ctx, c, cmd.Show.Tag)
	case cmd.AddJSON != nil:
		return printAddedServers(c.AddServersByJSON(ctx, cmd.AddJSON.Config))
	case cmd.AddURL != nil:
		return printAddedServers(c.AddServersByURL(ctx, cmd.AddURL.URLs, cmd.AddURL.SkipCertVerify))
	case cmd.Remove != nil:
		return c.RemoveServers(ctx, cmd.Remove.Tags)
	case cmd.PrivateServer != nil:
		return runPrivateServer(ctx, c, cmd.PrivateServer)
	case cmd.List != nil:
		return serversList(ctx, c, cmd.List.Latency, cmd.List.JSON)
	default:
		return serversList(ctx, c, false, false)
	}
}

func runPrivateServer(ctx context.Context, c *ipc.Client, cmd *PrivateServerCmd) error {
	switch {
	case cmd.Add != nil:
		return c.AddPrivateServer(ctx, cmd.Add.Tag, cmd.Add.IP, cmd.Add.Port, cmd.Add.Token)
	case cmd.Invite != nil:
		code, err := c.InviteToPrivateServer(ctx, cmd.Invite.IP, cmd.Invite.Port, cmd.Invite.Token, cmd.Invite.Name)
		if err != nil {
			return err
		}
		fmt.Println(code)
		return nil
	case cmd.RevokeInvite != nil:
		return c.RevokePrivateServerInvite(ctx, cmd.RevokeInvite.IP, cmd.RevokeInvite.Port, cmd.RevokeInvite.Token, cmd.RevokeInvite.Name)
	default:
		return fmt.Errorf("must specify one of: add, invite, revoke-invite")
	}
}

func serversList(ctx context.Context, c *ipc.Client, showLatency, asJSON bool) error {
	srvs, err := c.Servers(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		out := make([]ServerListEntry, 0, len(srvs))
		for _, s := range srvs {
			out = append(out, ServerListEntry{
				Tag:           s.Tag,
				Type:          s.Type,
				Location:      s.Location,
				URLTestResult: s.URLTestResult,
			})
		}
		return printJSON(out)
	}
	if len(srvs) == 0 {
		fmt.Println("No servers available")
		return nil
	}
	for _, s := range srvs {
		printServerEntry(s, showLatency)
	}
	return nil
}

func printServerEntry(s *servers.Server, showLatency bool) {
	fmt.Printf("  %s [%s]", s.Tag, s.Type)
	if s.Location != (C.ServerLocation{}) {
		fmt.Printf(" — %s, %s", s.Location.City, s.Location.Country)
	}
	if !showLatency {
		fmt.Println()
		return
	}
	if s.URLTestResult != nil {
		fmt.Printf(" (%dms)\n", s.URLTestResult.Delay)
	} else {
		fmt.Println(" (n/a)")
	}
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

func printAddedServers(tags []string, err error) error {
	if err != nil {
		return err
	}
	fmt.Printf("Added %d server(s): %s\n", len(tags), strings.Join(tags, ", "))
	return nil
}
