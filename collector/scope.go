package collector

import "net/netip"

// Address scopes. "shared" is the RFC 6598 carrier-grade NAT range
// (100.64.0.0/10): not routable on the internet, but not RFC 1918 either.
const (
	ScopePublic      = "public"
	ScopePrivate     = "private"
	ScopeShared      = "shared"
	ScopeLoopback    = "loopback"
	ScopeLinkLocal   = "link-local"
	ScopeMulticast   = "multicast"
	ScopeUnspecified = "unspecified"
	ScopeReserved    = "reserved"
)

func mustPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, len(values))
	for index, value := range values {
		prefixes[index] = netip.MustParsePrefix(value)
	}
	return prefixes
}

var (
	privatePrefixes = mustPrefixes(
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"fc00::/7", // unique local addresses
	)
	sharedPrefixes   = mustPrefixes("100.64.0.0/10")
	reservedPrefixes = mustPrefixes(
		"0.0.0.0/8",
		"192.0.0.0/24",
		"192.0.2.0/24",
		"198.18.0.0/15",
		"198.51.100.0/24",
		"203.0.113.0/24",
		"240.0.0.0/4",
		"100::/64",
		"2001:db8::/32",
		"fec0::/10", // deprecated site-local
	)
)

// ClassifyAddress returns the scope of an IPv4 or IPv6 address.
// IPv4-mapped IPv6 addresses are classified as their IPv4 address.
func ClassifyAddress(address netip.Addr) string {
	address = address.Unmap()
	switch {
	case !address.IsValid() || address.IsUnspecified():
		return ScopeUnspecified
	case address.IsLoopback():
		return ScopeLoopback
	case address.IsLinkLocalUnicast():
		return ScopeLinkLocal
	case address.IsMulticast():
		return ScopeMulticast
	case address == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return ScopeReserved
	}
	if contains(sharedPrefixes, address) {
		return ScopeShared
	}
	if contains(privatePrefixes, address) {
		return ScopePrivate
	}
	if contains(reservedPrefixes, address) {
		return ScopeReserved
	}
	return ScopePublic
}

func contains(prefixes []netip.Prefix, address netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}
