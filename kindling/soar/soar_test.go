package soar

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getlantern/soar"
	"github.com/stretchr/testify/require"
)

func TestUnconfiguredBuild(t *testing.T) {
	Zone, PublicKey = "", ""
	require.False(t, Configured())
	_, err := New(t.TempDir())
	require.Error(t, err)
}

func TestBadPublicKey(t *testing.T) {
	t.Cleanup(func() { Zone, PublicKey = "", "" })
	Zone, PublicKey = "t.example.com", "not-base64!"
	_, err := New(t.TempDir())
	require.Error(t, err)
}

func TestFileCacheRoundTrip(t *testing.T) {
	c := &fileCache{path: filepath.Join(t.TempDir(), "soar_resolvers.json")}
	require.Empty(t, c.Load())
	c.Store([]string{"1.2.3.4:53", "5.6.7.8:53"})
	require.Equal(t, []string{"1.2.3.4:53", "5.6.7.8:53"}, c.Load())
}

// A configured build reaches a Soar server and fetches through it.
func TestConfiguredBuildTunnels(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go soar.Serve(ctx, pc, soar.ServerConfig{Zone: "t.example.com", PrivateKey: priv,
		Dial: (&net.Dialer{}).DialContext})
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "through soar")
	}))
	t.Cleanup(hs.Close)

	t.Cleanup(func() { Zone, PublicKey = "", "" })
	Zone, PublicKey = "t.example.com", base64.StdEncoding.EncodeToString(pub)
	// Seed the per-device cache with the local server, as a returning user's would be: discovery
	// vets cached resolvers first.
	dataDir := t.TempDir()
	(&fileCache{path: filepath.Join(dataDir, "soar_resolvers.json")}).Store([]string{pc.LocalAddr().String()})

	sc, err := New(dataDir)
	require.NoError(t, err)
	defer sc.Close()
	rctx, rcancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer rcancel()
	rt, err := sc.NewRoundTripper(rctx, strings.TrimPrefix(hs.URL, "http://"))
	require.NoError(t, err)
	resp, err := (&http.Client{Transport: rt, Timeout: 20 * time.Second}).Get(hs.URL)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, "through soar", string(body))
}
