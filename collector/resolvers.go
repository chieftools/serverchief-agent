package collector

import (
	"context"
	"net/netip"
	"strings"
)

// ResolverInfo is the payload of the resolvers dataset. Nameserver and
// search order is significant, so these lists are kept in file order.
type ResolverInfo struct {
	Nameservers []string `json:"nameservers"`
	Search      []string `json:"search"`
	Options     []string `json:"options"`
	// ManagedBy names the tool that owns /etc/resolv.conf when it can be
	// told: systemd-resolved, resolvconf, NetworkManager, netconfig or "".
	ManagedBy       string           `json:"managed_by"`
	SystemdResolved *SystemdResolved `json:"systemd_resolved"`
}

// SystemdResolved holds the upstream servers behind the local stub.
type SystemdResolved struct {
	Stub        string   `json:"stub"`
	Nameservers []string `json:"nameservers"`
	Search      []string `json:"search"`
}

type resolvConf struct {
	nameservers []string
	search      []string
	options     []string
	header      string
}

// Resolvers collects the resolvers dataset.
type Resolvers struct {
	host *Host
}

func NewResolvers(host *Host) *Resolvers { return &Resolvers{host: host} }

func (*Resolvers) Name() string { return "resolvers" }

// systemdStubAddresses are the listeners of systemd-resolved's stub.
var systemdStubAddresses = map[string]bool{"127.0.0.53": true, "127.0.0.54": true}

func (collector *Resolvers) Collect(context.Context) (any, error) {
	host := collector.host
	info := &ResolverInfo{Nameservers: []string{}, Search: []string{}, Options: []string{}}

	data, err := host.readFile("/etc/resolv.conf")
	if err != nil {
		if ignorable(err) {
			return info, nil
		}
		return nil, err
	}
	parsed := parseResolvConf(data)
	info.Nameservers = parsed.nameservers
	info.Search = parsed.search
	info.Options = parsed.options
	info.ManagedBy = host.resolvConfManager(parsed.header)

	for _, nameserver := range parsed.nameservers {
		if !systemdStubAddresses[nameserver] {
			continue
		}
		resolved := &SystemdResolved{Stub: nameserver, Nameservers: []string{}, Search: []string{}}
		if upstream, err := host.readFile("/run/systemd/resolve/resolv.conf"); err == nil {
			upstreamParsed := parseResolvConf(upstream)
			resolved.Nameservers = upstreamParsed.nameservers
			resolved.Search = upstreamParsed.search
		}
		info.SystemdResolved = resolved
		if info.ManagedBy == "" {
			info.ManagedBy = "systemd-resolved"
		}
		break
	}
	return info, nil
}

func parseResolvConf(data []byte) resolvConf {
	parsed := resolvConf{nameservers: []string{}, search: []string{}, options: []string{}}
	var header []string
	for _, line := range lines(data) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			if len(header) < 5 {
				header = append(header, trimmed)
			}
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "nameserver":
			if address, err := netip.ParseAddr(strings.SplitN(fields[1], "%", 2)[0]); err == nil {
				parsed.nameservers = appendUnique(parsed.nameservers, address.String())
			}
		case "search":
			// The last search or domain line wins (resolv.conf(5)).
			parsed.search = []string{}
			for _, domain := range fields[1:] {
				if strings.HasPrefix(domain, "#") || strings.HasPrefix(domain, ";") {
					break
				}
				if domain = strings.TrimSuffix(domain, "."); domain != "" {
					parsed.search = appendUnique(parsed.search, domain)
				}
			}
		case "domain":
			parsed.search = []string{}
			if domain := strings.TrimSuffix(fields[1], "."); domain != "" {
				parsed.search = []string{domain}
			}
		case "options":
			for _, option := range fields[1:] {
				parsed.options = appendUnique(parsed.options, option)
			}
		}
	}
	parsed.header = strings.ToLower(strings.Join(header, "\n"))
	return parsed
}

func (host *Host) resolvConfManager(header string) string {
	if target, err := host.readLink("/etc/resolv.conf"); err == nil {
		switch {
		case strings.Contains(target, "systemd/resolve"):
			return "systemd-resolved"
		case strings.Contains(target, "resolvconf"):
			return "resolvconf"
		case strings.Contains(target, "NetworkManager"):
			return "NetworkManager"
		case strings.Contains(target, "netconfig"):
			return "netconfig"
		}
	}
	switch {
	case strings.Contains(header, "systemd-resolved"):
		return "systemd-resolved"
	case strings.Contains(header, "networkmanager"):
		return "NetworkManager"
	case strings.Contains(header, "resolvconf"):
		return "resolvconf"
	case strings.Contains(header, "netconfig"):
		return "netconfig"
	case strings.Contains(header, "cloud-init"):
		return "cloud-init"
	case strings.Contains(header, "docker"):
		return "docker"
	}
	return ""
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
