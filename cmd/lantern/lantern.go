package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"context"

	"github.com/alexflint/go-arg"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/ipc"
	"github.com/getlantern/radiance/issue"
	rlog "github.com/getlantern/radiance/log"
)

type args struct {
	Connect      *ConnectCmd      `arg:"subcommand:connect" help:"connect to VPN"`
	Disconnect   *DisconnectCmd   `arg:"subcommand:disconnect" help:"disconnect VPN"`
	Status       *StatusCmd       `arg:"subcommand:status" help:"show VPN status"`
	Servers      *ServersCmd      `arg:"subcommand:servers" help:"manage servers"`
	Features     *FeaturesCmd     `arg:"subcommand:features" help:"list available features and their status"`
	Set          *SetCmd          `arg:"subcommand:set" help:"update one or more settings"`
	Get          *GetCmd          `arg:"subcommand:get" help:"show one or all settings"`
	SplitTunnel  *SplitTunnelCmd  `arg:"subcommand:split-tunnel" help:"split-tunnel filter management"`
	Account      *AccountCmd      `arg:"subcommand:account" help:"login, signup, user data, devices, recovery"`
	Subscription *SubscriptionCmd `arg:"subcommand:subscription" help:"plans, payments, and billing"`
	ReportIssue  *ReportIssueCmd  `arg:"subcommand:report-issue" help:"report an issue"`
	Logs         *LogsCmd         `arg:"subcommand:logs" help:"tail daemon logs"`
	IP           *IPCmd           `arg:"subcommand:ip" help:"show public IP address"`
	Version      *VersionCmd      `arg:"subcommand:version" help:"print version"`
}

func (args) Description() string {
	return "Lantern CLI — command-line interface for the Lantern VPN daemon"
}

type ReportIssueCmd struct {
	Type        int    `arg:"-t,--type,required" help:"0=purchase 1=signin 2=spinner 3=blocked-sites 4=slow 5=link-device 6=crash 9=other 10=update"`
	Description string `arg:"-d,--desc,required" help:"issue description"`
	Email       string `arg:"-e,--email" help:"email address"`
}

func runReportIssue(ctx context.Context, c *ipc.Client, cmd *ReportIssueCmd) error {
	return c.ReportIssue(ctx, issue.IssueType(cmd.Type), cmd.Description, cmd.Email, nil)
}

type LogsCmd struct{}

func tailLogs(ctx context.Context, c *ipc.Client) error {
	err := c.TailLogs(ctx, func(entry rlog.LogEntry) {
		fmt.Println(entry)
	})
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "\nStopped tailing logs.")
		return nil
	}
	return err
}

type VersionCmd struct{}

func main() {
	var a args
	p := arg.MustParse(&a)
	if p.Subcommand() == nil {
		p.WriteHelp(os.Stdout)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client := ipc.NewClient()
	defer client.Close()

	if err := run(ctx, client, &a); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n\n", err)
		p.WriteHelpForSubcommand(os.Stdout, p.SubcommandNames()...)
		os.Exit(1)
	}
}

func run(ctx context.Context, c *ipc.Client, a *args) error {
	switch {
	case a.Connect != nil:
		return vpnConnect(ctx, c, a.Connect.Name, a.Connect.Wait)
	case a.Disconnect != nil:
		return c.DisconnectVPN(ctx)
	case a.Status != nil:
		return vpnStatus(ctx, c)
	case a.Servers != nil:
		return runServers(ctx, c, a.Servers)
	case a.Features != nil:
		return runFeatures(ctx, c)
	case a.Set != nil:
		return runSet(ctx, c, a.Set)
	case a.Get != nil:
		return runGet(ctx, c, a.Get)
	case a.SplitTunnel != nil:
		return runSplitTunnel(ctx, c, a.SplitTunnel)
	case a.Account != nil:
		return runAccount(ctx, c, a.Account)
	case a.Subscription != nil:
		return runSubscription(ctx, c, a.Subscription)
	case a.ReportIssue != nil:
		return runReportIssue(ctx, c, a.ReportIssue)
	case a.Logs != nil:
		return tailLogs(ctx, c)
	case a.IP != nil:
		return runIP(ctx)
	case a.Version != nil:
		fmt.Println(common.Version)
		return nil
	default:
		return fmt.Errorf("no subcommand specified")
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
