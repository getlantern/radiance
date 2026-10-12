//go:build !windows

package main

import (
	"context"
	"errors"

	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common/legacymigration"
	"github.com/getlantern/radiance/ipc"
)

func prepareLegacyMigration(*prepareLegacyMigrationCmd) error {
	return errors.New("legacy migration requires Windows")
}

func bootstrapLegacyMigration(context.Context, string, string) (*legacymigration.Controller, func() error, error) {
	return nil, func() error { return nil }, nil
}

func completeLegacyMigration(*legacymigration.Controller, *ipc.Server, *backend.LocalBackend) error {
	return errors.New("legacy migration requires Windows")
}
