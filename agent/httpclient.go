package agent

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"time"

	"golang.org/x/crypto/x509roots/fallback/bundle"
)

// RootCAs returns the system roots plus the embedded Mozilla bundle. The
// standard library only falls back to embedded roots when the system has
// none at all; old distributions (Ubuntu 14.04, CentOS 7) do have a bundle,
// it is just stale, so the embedded roots are added unconditionally.
func RootCAs() *x509.CertPool {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	for root := range bundle.Roots() {
		certificate, err := x509.ParseCertificate(root.Certificate)
		if err != nil {
			continue
		}
		if root.Constraint == nil {
			pool.AddCert(certificate)
		} else {
			pool.AddCertWithConstraint(certificate, root.Constraint)
		}
	}
	return pool
}

// NewHTTPClient returns the client used for backend requests: proxy aware,
// TLS 1.2+, with RootCAs and conservative timeouts.
func NewHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			TLSClientConfig:       &tls.Config{RootCAs: RootCAs(), MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          2,
			ForceAttemptHTTP2:     true,
		},
	}
}
