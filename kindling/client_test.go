package kindling

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewClient(t *testing.T) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelDebug,
	})))
	k = NewKindling()
	SetKindling(k)
	defer Close(context.Background())
	cli := HTTPClient()
	assert.NotNil(t, cli)

	req, err := http.NewRequest(http.MethodGet, "https://google.com", http.NoBody)
	assert.NoError(t, err)

	resp, err := cli.Do(req)
	assert.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	content, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	t.Logf("content: %s", content)
}
