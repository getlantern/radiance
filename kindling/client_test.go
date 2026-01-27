package kindling

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"testing"

	"github.com/getlantern/radiance/common/settings"
	"github.com/stretchr/testify/assert"
)

func TestNewClient(t *testing.T) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelDebug,
	})))
	settings.Set(settings.DataPathKey, t.TempDir())
	k = NewKindling()
	SetKindling(k)
	defer Close(context.Background())
	cli := HTTPClient()
	assert.NotNil(t, cli)

	req, err := http.NewRequest(http.MethodGet, "https://www.gstatic.com/generate_204", http.NoBody)
	assert.NoError(t, err)

	resp, err := cli.Do(req)
	assert.NoError(t, err)

	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}
