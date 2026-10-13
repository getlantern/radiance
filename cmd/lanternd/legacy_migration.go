package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/getlantern/radiance/common"
	commonenv "github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/legacymigration"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
	rlog "github.com/getlantern/radiance/log"
)

func initializeDaemonDiagnostics(dataPath, logPath, logLevel string) error {
	dataPath = withDefault(commonenv.GetString(commonenv.DataPath), dataPath)
	logPath = withDefault(commonenv.GetString(commonenv.LogPath), logPath)
	if err := settings.SetRuntimePaths(dataPath, logPath); err != nil {
		return err
	}
	if err := os.MkdirAll(logPath, 0755); err != nil {
		return err
	}
	output := filepath.Join(logPath, internal.LogFileName)
	if commonenv.GetBool(commonenv.LogToStdout) {
		output = rlog.StdoutPath
	}
	slog.SetDefault(rlog.NewLogger(rlog.Config{LogPath: output, Level: logLevel, Prod: common.Prod()}))
	return nil
}

func waitForLegacyAdoption(ctx context.Context, migration *legacymigration.Controller, initialTimeout time.Duration) (*legacymigration.Record, error) {
	initial, cancel := context.WithTimeout(ctx, initialTimeout)
	record, err := migration.Wait(initial)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return record, err
	}
	slog.Warn("Legacy adoption is pending; diagnostics and authenticated retry remain available")
	return migration.Wait(ctx)
}
