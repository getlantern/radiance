package main

import (
	"context"
	"fmt"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/ipc"
	rlog "github.com/getlantern/radiance/log"
)

type FeaturesCmd struct{}

func runFeatures(ctx context.Context, c *ipc.Client) error {
	f, err := c.Features(ctx)
	if err != nil {
		return err
	}
	for k, v := range f {
		fmt.Printf("%s: %v\n", k, v)
	}
	return nil
}

// settingViews is the single source of truth for which settings the CLI exposes
// under `set`/`get` and how their user-facing values map to the underlying
// settings keys.
var settingViews = []struct {
	name string
	get  func(settings.Settings) any
}{
	{"smart-routing", func(s settings.Settings) any { return orBool(s[settings.SmartRoutingKey]) }},
	{"ad-block", func(s settings.Settings) any { return orBool(s[settings.AdBlockKey]) }},
	{"telemetry", func(s settings.Settings) any { return orBool(s[settings.TelemetryKey]) }},
	{"split-tunnel", func(s settings.Settings) any { return orBool(s[settings.SplitTunnelKey]) }},
	{"config-fetch", func(s settings.Settings) any { return !toBool(s[settings.ConfigFetchDisabledKey]) }},
	{"log-level", func(s settings.Settings) any { return orString(s[settings.LogLevelKey]) }},
}

type SetCmd struct {
	SmartRouting *bool   `arg:"--smart-routing" help:"enable or disable smart routing (true|false)"`
	AdBlock      *bool   `arg:"--ad-block" help:"enable or disable ad blocking (true|false)"`
	Telemetry    *bool   `arg:"--telemetry" help:"enable or disable telemetry (true|false)"`
	SplitTunnel  *bool   `arg:"--split-tunnel" help:"enable or disable split tunneling (true|false)"`
	ConfigFetch  *bool   `arg:"--config-fetch" help:"enable or disable periodic config fetching (true|false)"`
	LogLevel     *string `arg:"--log-level" help:"log level (trace|debug|info|warn|error|fatal|panic|disable)"`
}

func runSet(ctx context.Context, c *ipc.Client, cmd *SetCmd) error {
	updates := settings.Settings{}
	if cmd.SmartRouting != nil {
		updates[settings.SmartRoutingKey] = *cmd.SmartRouting
	}
	if cmd.AdBlock != nil {
		updates[settings.AdBlockKey] = *cmd.AdBlock
	}
	if cmd.Telemetry != nil {
		updates[settings.TelemetryKey] = *cmd.Telemetry
	}
	if cmd.SplitTunnel != nil {
		updates[settings.SplitTunnelKey] = *cmd.SplitTunnel
	}
	if cmd.ConfigFetch != nil {
		updates[settings.ConfigFetchDisabledKey] = !*cmd.ConfigFetch
	}
	if cmd.LogLevel != nil {
		if _, err := rlog.ParseLogLevel(*cmd.LogLevel); err != nil {
			return err
		}
		updates[settings.LogLevelKey] = *cmd.LogLevel
	}
	if len(updates) == 0 {
		return fmt.Errorf("no settings provided; pass one or more flags (see `lantern set --help`)")
	}
	_, err := c.PatchSettings(ctx, updates)
	return err
}

type GetCmd struct {
	Name string `arg:"positional" help:"setting name (smart-routing, ad-block, telemetry, split-tunnel, config-fetch, log-level); omit to list all"`
}

func runGet(ctx context.Context, c *ipc.Client, cmd *GetCmd) error {
	s, err := c.Settings(ctx)
	if err != nil {
		return err
	}
	if cmd.Name == "" {
		for _, v := range settingViews {
			fmt.Printf("%s: %v\n", v.name, v.get(s))
		}
		return nil
	}
	for _, v := range settingViews {
		if v.name == cmd.Name {
			fmt.Printf("%s: %v\n", v.name, v.get(s))
			return nil
		}
	}
	return fmt.Errorf("unknown setting %q", cmd.Name)
}

func orBool(v any) any {
	if v == nil {
		return false
	}
	return v
}

func orString(v any) any {
	if v == nil {
		return ""
	}
	return v
}

func toBool(v any) bool {
	if v == nil {
		return false
	}
	return fmt.Sprintf("%v", v) == "true"
}
