package ipc

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func TestWindowsPipeHTTPAuthenticatesAfterReadingAndSurvivesAnonymousClient(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	path := fmt.Sprintf(`\\.\pipe\Lantern-IPC-Test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	raw, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: `D:P(A;;GA;;;` + sid + `)`})
	if err != nil {
		t.Fatal(err)
	}
	listener := &winioListener{Listener: raw}
	type connectionKey struct{}
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			if _, err := getConnPeer(conn); err == nil {
				t.Error("authenticated peer before reading client data")
			}
			return context.WithValue(ctx, connectionKey{}, conn)
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			peer, err := getConnPeer(r.Context().Value(connectionKey{}).(net.Conn))
			if err != nil || peer.uid != sid {
				http.Error(w, "wrong peer", http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, peer.uid)
		}),
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() { server.Close(); <-done })
	for _, level := range []winio.PipeImpLevel{winio.PipeImpLevelAnonymous, winio.PipeImpLevelIdentification} {
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return winio.DialPipeAccessImpLevel(ctx, path, pipeClientAccess, level)
		}}
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		response, err := client.Get("http://pipe/peer")
		if level == winio.PipeImpLevelAnonymous {
			if err == nil {
				response.Body.Close()
				t.Fatal("anonymous HTTP client received a response")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK || string(body) != sid {
				t.Fatalf("HTTP peer response = %d %q, %v", response.StatusCode, body, err)
			}
		}
		transport.CloseIdleConnections()
	}
}
