// Package trust builds the root certificate pool the agent trusts: the
// system roots plus the embedded Mozilla bundle.
package trust

import (
	"crypto/x509"

	"golang.org/x/crypto/x509roots/fallback/bundle"
)

// Roots returns a new pool with the system roots plus the embedded Mozilla
// bundle. The standard library only falls back to embedded roots when the
// system has none at all; old distributions (Ubuntu 14.04, CentOS 7) do
// have a bundle, it is just stale, so the embedded roots are added
// unconditionally.
func Roots() *x509.CertPool {
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
