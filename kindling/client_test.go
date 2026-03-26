package kindling

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewClient(t *testing.T) {
	k = NewKindling(t.TempDir())
	SetKindling(k)

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
