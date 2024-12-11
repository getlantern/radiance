package config

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewConfigHandler(t *testing.T) {
	ch := NewConfigHandler()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg, err := ch.GetConfig(ctx)
	require.NoError(t, err)
	t.Logf("Config: %v", cfg)
}
