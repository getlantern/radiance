package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/getlantern/radiance/common/fileperm"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/vpn"
)

func run(ctx context.Context) error {
	dir, target := os.Getenv("DATA"), os.Getenv("TARGET_URL")
	if dir == "" || target == "" {
		return errors.New("DATA and TARGET_URL are required")
	}
	for _, name := range []string{"success", "fetch.json", "connection.txt", "output.txt", "timing.txt"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	seconds := 20
	if value := os.Getenv("FETCH_TIMEOUT_SECONDS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 || n > 600 {
			return errors.New("FETCH_TIMEOUT_SECONDS must be between 1 and 600")
		}
		seconds = n
	}
	timeout := time.Duration(seconds) * time.Second
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if os.Getenv("DIRECT") == "true" {
		return fetch(ctx, target, "", dir, timeout)
	}
	if env := os.Getenv("RADIANCE_ENV"); env != "staging" && env != "production" {
		return errors.New("RADIANCE_ENV must explicitly select staging or production")
	}
	sticky := os.Getenv("STICKY") == "true"
	tag := os.Getenv("TARGET_TAG")
	if sticky {
		if tag == "" {
			return errors.New("TARGET_TAG is required with STICKY")
		}
		if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
			return fmt.Errorf("sticky config: %w", err)
		}
		if err := os.Setenv("RADIANCE_DISABLE_FETCH_CONFIG", "true"); err != nil {
			return err
		}
	} else {
		tag = vpn.AutoSelectTag
	}
	uid, err := strconv.ParseInt(os.Getenv("USER_ID"), 10, 64)
	if err != nil || uid <= 0 || os.Getenv("TOKEN") == "" || os.Getenv("DEVICE_ID") == "" {
		return errors.New("USER_ID, TOKEN and DEVICE_ID are required")
	}
	be, err := backend.NewLocalBackend(ctx, backend.Options{DataDir: dir, LogDir: dir, Locale: "en-US"})
	if err != nil {
		return fmt.Errorf("create radiance: %w", err)
	}
	defer be.Close()
	settings.Set(settings.UserIDKey, uid)
	settings.Set(settings.TokenKey, os.Getenv("TOKEN"))
	settings.Set(settings.DevicesKey, []settings.Device{{ID: os.Getenv("DEVICE_ID"), Name: "pinger"}})
	settings.Set(settings.ConfigFetchDisabledKey, sticky)
	be.Start()
	if sticky {
		if _, found := be.GetServerByTag(tag); !found {
			return errors.New("assigned target is absent from sticky configuration")
		}
	}
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := os.WriteFile(filepath.Join(dir, "connection.txt"), []byte("connecting"), fileperm.Private); err != nil {
		return err
	}
	if err := be.ConnectVPN(connectCtx, tag); err != nil {
		return fmt.Errorf("connect assigned target: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "connection.txt"), []byte("connected"), fileperm.Private); err != nil {
		return err
	}
	defer be.DisconnectVPN()
	proxy := os.Getenv("RADIANCE_SOCKS_ADDRESS")
	if proxy == "" {
		proxy = "127.0.0.1:1080"
	}
	return fetch(ctx, target, proxy, dir, timeout)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	seconds, _ := strconv.Atoi(os.Getenv("FETCH_TIMEOUT_SECONDS"))
	if seconds <= 0 || seconds > 600 {
		seconds = 20
	}
	ctx, deadline := context.WithTimeout(ctx, time.Duration(2*seconds+15)*time.Second)
	defer deadline()
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			return
		}
		fmt.Fprintln(os.Stderr, err)
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, ctx.Err())
	}
	os.Exit(1)
}
