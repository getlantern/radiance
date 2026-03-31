package portforward

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalIP(t *testing.T) {
	ip, err := LocalIP()
	require.NoError(t, err)
	assert.NotEmpty(t, ip)
	// Should be a private network IP
	assert.Regexp(t, `^(10\.|172\.(1[6-9]|2\d|3[01])\.|192\.168\.)`, ip)
}

func TestRandomPort(t *testing.T) {
	seen := make(map[uint16]bool)
	for i := 0; i < 100; i++ {
		p := randomPort()
		assert.GreaterOrEqual(t, p, uint16(portRangeMin))
		assert.Less(t, p, uint16(portRangeMax))
		seen[p] = true
	}
	// With 100 draws from a 50000-range, we should get many distinct values
	assert.Greater(t, len(seen), 50)
}

func TestNewForwarder(t *testing.T) {
	f := New()
	assert.NotNil(t, f)
	assert.Nil(t, f.Active())
}

func TestUnmapPort_NoMapping(t *testing.T) {
	f := New()
	// Should be a no-op, not an error
	err := f.UnmapPort(context.Background())
	assert.NoError(t, err)
}

func TestStartRenewal_NoMapping(t *testing.T) {
	f := New()
	// Should be a no-op when there's no active mapping
	f.StartRenewal(context.Background())
	assert.Nil(t, f.stopC) // no goroutine started
}
