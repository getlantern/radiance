package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/kindling"
)

func performKindlingPing(ctx context.Context, urlToHit string, runID string, deviceID string, userID int64, token string, dataDir string) error {
	os.MkdirAll(dataDir, 0o755)
	settings.Set(settings.DataPathKey, dataDir)
	settings.Set(settings.UserIDKey, userID)
	settings.Set(settings.TokenKey, token)
	settings.Set(settings.UserLevelKey, "")
	settings.Set(settings.EmailKey, "pinger@pinger.com")
	settings.Set(settings.DevicesKey, []settings.Device{
		{
			ID:   deviceID,
			Name: deviceID,
		},
	})

	t1 := time.Now()
	kindling.SetKindling(kindling.NewKindling())
	defer kindling.Close(ctx)
	cli := kindling.HTTPClient()

	t2 := time.Now()
	// Run the command and capture the output
	resp, err := cli.Get(urlToHit)
	if err != nil {
		slog.Error("failed on get request", slog.Any("error", err))
		return err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("failed to read response body", slog.Any("error", err))
		return err
	}
	t3 := time.Now()
	slog.Info("lantern ping completed successfully")
	// create a marker file that will be used by the pinger to determine success
	if err := os.WriteFile(dataDir+"/success", []byte(""), 0o644); err != nil {
		slog.Error("failed to write success file", slog.Any("error", err), slog.String("path", dataDir+"/success"))
	}
	if err := os.WriteFile(dataDir+"/output.txt", responseBody, 0o644); err != nil {
		slog.Error("failed to write output file", slog.Any("error", err), slog.String("path", dataDir+"/output.txt"))
	}
	return os.WriteFile(dataDir+"/timing.txt", []byte(fmt.Sprintf(`
	result: %v
	run-id: %s
	err: %v
	started: %s
	connected: %d
	fetched: %d
	url: %s`,
		true, runID, nil, t1, int32(t2.Sub(t1).Milliseconds()), int32(t3.Sub(t1).Milliseconds()), urlToHit)), 0o644)
}

func main() {
	deviceID := os.Getenv("DEVICE_ID")
	userID := os.Getenv("USER_ID")
	token := os.Getenv("TOKEN")
	runID := os.Getenv("RUN_ID")
	targetURL := os.Getenv("TARGET_URL")
	data := os.Getenv("DATA")
	transport := os.Getenv("TRANSPORT")

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	if deviceID == "" || runID == "" || targetURL == "" || data == "" || transport == "" {
		slog.Error("missing required environment variable(s), required environment variables: DEVICE_ID, RUN_ID, TARGET_URL, DATA, TRANSPORT", slog.String("deviceID", deviceID), slog.String("runID", runID), slog.String("targetURL", targetURL), slog.String("data", data), slog.String("transport", transport))
		os.Exit(1)
	}

	var uid int64

	if userID != "" {
		var err error
		uid, err = strconv.ParseInt(userID, 10, 64)
		if err != nil {
			slog.Error("failed to parse USER_ID", slog.Any("error", err))
			os.Exit(1)
		}
	}

	ctx := context.Background()

	kindling.EnabledTransports[transport] = true
	slog.Debug("enabled transports", slog.Any("enabled_transports", kindling.EnabledTransports))
	if err := performKindlingPing(ctx, targetURL, runID, deviceID, uid, token, data); err != nil {
		slog.Error("failed to perform kindling ping", slog.Any("error", err))
		os.Exit(1)
	}
}
