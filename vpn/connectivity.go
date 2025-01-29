package vpn

import (
	"bufio"
	"context"
	"fmt"
	"net/http"

	"github.com/Jigsaw-Code/outline-sdk/transport"
)

func checkConnectivity(ctx context.Context, dialer transport.StreamDialer, target, authToken string) error {
	log.Debugf("connection test --> dialing target %s", target)
	conn, err := dialer.DialStream(ctx, target)
	if err != nil {
		return err
	}
	defer conn.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodConnect, target, nil)
	if err != nil {
		return fmt.Errorf("connection test --> failed to create request: %w", err)
	}
	req.Header.Set(authTokenHeader, authToken)

	if err = req.Write(conn); err != nil {
		return fmt.Errorf("connection test --> failed to write request: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return fmt.Errorf("connection test --> failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("connection test --> CONNECT request failed: %s", resp.Status)
	}
	log.Debug("connection test --> connection successful")
	return nil
}
