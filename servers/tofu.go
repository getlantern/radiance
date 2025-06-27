package servers

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"time"
)

type CertDetail struct {
	Subject     string
	Issuer      string
	Fingerprint string
}

// this file contains the code for Trust On First Use (TOFU) when accessing lantern server manager
var (
	ErrNoMatchingFingerprint = errors.New("server certificate(s) didn't match trusted fingerprint")
	ErrCertExpired           = errors.New("server certificate has expired")
	ErrCertNotYetValid       = errors.New("server certificate is not yet valid")
	ErrNoCertsDetected       = errors.New("no server certificates detected")
	ErrTrustCancelled        = errors.New("user cancelled the trust dialog")
)

// getFingerprint returns the SHA1 fingerprint of the given DER-encoded certificate.
func getFingerprint(der []byte) string {
	hash := sha1.Sum(der)
	hexified := make([][]byte, len(hash))
	for i, data := range hash {
		hexified[i] = []byte(fmt.Sprintf("%02X", data))
	}
	return string(bytes.Join(hexified, []byte(":")))
}

// getTOFUClient return an http client that will verify a match given a trusted fingerprint,
func getTOFUClient(fingerprint string) (*http.Client, error) {
	slog.Debug("Attempting connection with trusted fingerprint", "fingerprint", fingerprint)
	dial := func(_ context.Context, network, addr string) (net.Conn, error) {
		config := &tls.Config{
			InsecureSkipVerify: true, // We'll verify ourselves
		}

		conn, err := tls.Dial(network, addr, config)
		if err != nil {
			slog.Error("Unable to connect", "addr", addr, "err", err)
			return nil, err

		}
		state := conn.ConnectionState()
		now := time.Now()
		matched := false
		for _, cert := range state.PeerCertificates {
			if fingerprint == getFingerprint(cert.Raw) {
				matched = true
			}
			if now.Before(cert.NotBefore) {
				_ = conn.Close()
				return nil, ErrCertNotYetValid
			}
			if now.After(cert.NotAfter) {
				_ = conn.Close()
				return nil, ErrCertExpired
			}
		}
		if !matched {
			_ = conn.Close()
			return nil, ErrNoMatchingFingerprint
		}
		// If we've gotten this far, then we can trust the server
		slog.Debug("Server cert(s) passed TOFU tests")
		return conn, nil
	}
	return &http.Client{
		Transport: &http.Transport{
			DialTLSContext: dial,
		},
	}, nil
}

// getServerFingerprints connects to an untrusted server, and get cert information about it
func getServerFingerprints(host string, port int) ([]CertDetail, error) {
	var ret []CertDetail

	addr := fmt.Sprintf("%s:%d", host, port)
	slog.Debug("Performing TOFU check", "addr", addr)

	config := &tls.Config{
		InsecureSkipVerify: true, // By definition, we're trying to build the trust...
	}
	conn, err := tls.Dial("tcp", addr, config)
	if err != nil {
		slog.Error("Unable to connect to", "addr", addr, "err", err)
		return nil, err
	} else {
		state := conn.ConnectionState()

		for _, cert := range state.PeerCertificates {
			ret = append(ret, CertDetail{
				Subject:     cert.Subject.CommonName,
				Issuer:      cert.Issuer.CommonName,
				Fingerprint: getFingerprint(cert.Raw),
			})
		}
		if len(ret) == 0 {
			return nil, ErrNoCertsDetected
		}
		return ret, nil
	}
}

// TrustFingerprintCallback is a callback function that is called when a server's certificate
// fingerprint is detected. It receives the server's IP address and the certificate details.
// The responder can return a CertDetail to be used as the trusted fingerprint.
// If the returned CertDetail is nil, the connection will be rejected.
type TrustFingerprintCallback func(ip string, details []CertDetail) *CertDetail

func readTrustedServerFingerprints(fingerprintsJSON string) (map[string]string, error) {
	fingerprints := map[string]string{}

	data, err := os.ReadFile(fingerprintsJSON)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fingerprints, nil
		}
		return nil, fmt.Errorf("failed to read trusted server fingerprints: %w", err)
	}

	if err := json.Unmarshal(data, &fingerprints); err != nil {
		return nil, fmt.Errorf("failed to unmarshal trusted server fingerprints: %w", err)
	}

	return fingerprints, nil
}

func writeTrustedServerFingerprints(fingerprintsJSON string, fingerprints map[string]string) error {
	data, err := json.Marshal(fingerprints)
	if err != nil {
		return fmt.Errorf("failed to marshal trusted server fingerprints: %w", err)
	}

	if err := os.WriteFile(fingerprintsJSON, data, 0600); err != nil {
		return fmt.Errorf("failed to write trusted server fingerprints: %w", err)
	}
	return nil
}

// getTrustedServerFingerprint returns all trusted fingerprints and the trusted fingerprint for the given IP
func getTrustedServerFingerprint(fingerprintsJSON string, ip string, details []CertDetail) (map[string]string, string, error) {
	fingerprints, err := readTrustedServerFingerprints(fingerprintsJSON)
	if err != nil {
		return fingerprints, "", fmt.Errorf("failed to read trusted server fingerprints: %w", err)
	}
	if trustedFingerprint, exists := fingerprints[ip]; exists {
		if slices.ContainsFunc(details, func(cert CertDetail) bool {
			return cert.Fingerprint == trustedFingerprint
		}) {
			return fingerprints, trustedFingerprint, nil
		}
		return fingerprints, "", fmt.Errorf("fingerprint mismatch: expected %s", trustedFingerprint)
	}

	return fingerprints, "", nil
}
