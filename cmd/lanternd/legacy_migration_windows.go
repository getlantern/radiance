package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/getlantern/radiance/account"
	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common/legacymigration"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/ipc"
	"github.com/getlantern/radiance/kindling"
	"github.com/getlantern/radiance/vpn"
)

func prepareLegacyMigration(command *prepareLegacyMigrationCmd) error {
	return legacymigration.Prepare(command.SID, command.Source, command.MigrationID)
}

func bootstrapLegacyMigration(ctx context.Context, dataPath, proURL string) (*legacymigration.Controller, func() error, error) {
	closeNothing := func() error { return nil }
	record, err := legacymigration.Load()
	if err != nil || record == nil {
		return nil, closeNothing, err
	}
	if !strings.EqualFold(filepath.Clean(dataPath), filepath.Clean(internal.DefaultDataPath())) {
		return nil, closeNothing, errors.New("migration requires the protected service data directory")
	}
	if err := legacymigration.ProtectDataDir(dataPath); err != nil {
		return nil, closeNothing, err
	}
	store, err := legacymigration.OpenStore()
	if err != nil {
		return nil, closeNothing, err
	}
	controller, err := legacymigration.NewController(store, func(ctx context.Context, request legacymigration.Request) (json.RawMessage, string, error) {
		data, err := account.VerifyLegacyIdentity(ctx, kindling.HTTPClient(), proURL, request.UserID, request.Token, request.DeviceID)
		if err != nil {
			return nil, "", err
		}
		raw, err := json.Marshal(data)
		return raw, data.LegacyUserData.UserLevel, err
	})
	if err != nil {
		return nil, closeNothing, err
	}
	closeServer, err := controller.Start(ctx)
	if err != nil {
		return nil, closeNothing, err
	}
	record, err = controller.Wait(ctx)
	if err == nil {
		err = legacymigration.WriteSettings(dataPath, record)
	}
	if err != nil {
		_ = closeServer()
		return nil, closeNothing, err
	}
	return controller, closeServer, nil
}

func completeLegacyMigration(controller *legacymigration.Controller, server *ipc.Server, be *backend.LocalBackend) error {
	controller.SetRollback(func(ctx context.Context) error {
		if !server.Running() {
			return errors.New("backend is unavailable")
		}
		if err := be.DisconnectVPN(); err != nil {
			return errors.New("destination disconnect failed")
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			if be.VPNStatus() == vpn.Disconnected {
				return nil
			}
			select {
			case <-ctx.Done():
				return errors.New("destination disconnect was not confirmed")
			case <-ticker.C:
			}
		}
	})
	return controller.MarkReady(func() legacymigration.State {
		state := legacymigration.State{
			UserID: settings.GetInt64(settings.UserIDKey), Token: settings.GetString(settings.TokenKey),
			DeviceID: settings.GetString(settings.DeviceIDKey), Locale: settings.GetString(settings.LocaleKey),
			UserLevel: settings.GetString(settings.UserLevelKey), AutoReport: settings.GetBool(settings.TelemetryKey),
			ProxyAll: !settings.GetBool(settings.SmartRoutingKey), AutoLaunch: settings.GetBool(settings.LegacyAutoLaunchKey),
		}
		user, err := be.UserData()
		state.Healthy = server.Running() && err == nil && user != nil && user.LegacyUserData != nil &&
			user.LegacyID == state.UserID && user.LegacyToken == state.Token &&
			user.LegacyUserData.UserId == state.UserID && user.LegacyUserData.DeviceID == state.DeviceID &&
			user.LegacyUserData.UserLevel == state.UserLevel
		return state
	})
}
