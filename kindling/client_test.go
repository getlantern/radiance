package kindling

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClient(t *testing.T) {
	newK, err := NewKindling(t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, newK)
	SetKindling(newK)

	t.Cleanup(func() {
		Close()
		k = nil
	})

	cli := HTTPClient()
	assert.NotNil(t, cli)

	req, err := http.NewRequest(http.MethodGet, "https://www.gstatic.com/generate_204", http.NoBody)
	assert.NoError(t, err)

	resp, err := cli.Do(req)
	assert.NoError(t, err)

	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}
