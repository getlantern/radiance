package fronted

import (
	"bytes"
	"testing"

	"github.com/getlantern/domainfront"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEmbeddedConfigValid guards the offline-boot guarantee: NewFronted seeds
// domainfront with the embedded config, so a fresh install with configURL
// blocked still boots — which requires the embedded copy to parse and carry
// providers. (Persistence and bootstrap-preference are tested in domainfront.)
func TestEmbeddedConfigValid(t *testing.T) {
	cfg, err := domainfront.ParseConfigFromReader(bytes.NewReader(embeddedConfig))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.NotEmpty(t, cfg.Providers, "embedded fronted config must contain providers")
}
