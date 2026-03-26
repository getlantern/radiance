package main

import (
	"context"
	"fmt"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/ipc"
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

type SmartRoutingCmd struct {
	Enable *bool `arg:"positional" help:"enable or disable smart routing (true|false)"`
}

func runSmartRouting(ctx context.Context, c *ipc.Client, cmd *SmartRoutingCmd) error {
	if cmd.Enable == nil {
		s, err := c.Settings(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Smart routing: %v\n", toBool(s[settings.SmartRoutingKey]))
		return nil
	}
	if err := c.EnableSmartRouting(ctx, *cmd.Enable); err != nil {
		return err
	}
	fmt.Printf("Smart routing set to %v\n", *cmd.Enable)
	return nil
}

type AdBlockCmd struct {
	Enable *bool `arg:"positional" help:"enable or disable ad blocking (true|false)"`
}

func runAdBlock(ctx context.Context, c *ipc.Client, cmd *AdBlockCmd) error {
	if cmd.Enable == nil {
		s, err := c.Settings(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Ad blocking: %v\n", toBool(s[settings.AdBlockKey]))
		return nil
	}
	if err := c.EnableAdBlocking(ctx, *cmd.Enable); err != nil {
		return err
	}
	fmt.Printf("Ad blocking set to %v\n", *cmd.Enable)
	return nil
}

type TelemetryCmd struct {
	Enable *bool `arg:"positional" help:"enable or disable telemetry (true|false)"`
}

func runTelemetry(ctx context.Context, c *ipc.Client, cmd *TelemetryCmd) error {
	if cmd.Enable == nil {
		s, err := c.Settings(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Telemetry: %v\n", toBool(s[settings.TelemetryKey]))
		return nil
	}
	if err := c.EnableTelemetry(ctx, *cmd.Enable); err != nil {
		return err
	}
	fmt.Printf("Telemetry set to %v\n", *cmd.Enable)
	return nil
}

func toBool(v any) bool {
	if v == nil {
		return false
	}
	return fmt.Sprintf("%v", v) == "true"
}
