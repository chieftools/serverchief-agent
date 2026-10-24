package agent

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"go.chief.tools/serverchief-agent/trust"
)

// RootCAs returns the system roots plus the embedded Mozilla bundle
// (trust.Roots).
func RootCAs() *x509.CertPool {
	return trust.Roots()
}

// TLSConfig returns the TLS settings for backend and realtime connections:
// TLS 1.2+, RootCAs, plus the PEM certificates in caFile when it is set
// (a local development CA such as Laravel Valet's).
func TLSConfig(caFile string) (*tls.Config, error) {
	pool := RootCAs()
	if caFile != "" {
		data, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		if !pool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("CA file %s contains no PEM certificates", caFile)
		}
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// NewHTTPClient returns the client used for backend requests: proxy aware,
// with TLSConfig(caFile) and conservative timeouts.
func NewHTTPClient(caFile string) (*http.Client, error) {
	tlsConfig, err := TLSConfig(caFile)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			TLSClientConfig:       tlsConfig,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          2,
			ForceAttemptHTTP2:     true,
		},
	}, nil
}
