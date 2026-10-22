package collector

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// NetworkInfo is the payload of the network dataset.
type NetworkInfo struct {
	Interfaces    []Interface    `json:"interfaces"`
	DefaultRoutes []DefaultRoute `json:"default_routes"`
}

// Interface is one network interface.
type Interface struct {
	Name string `json:"name"`
	// Kind is loopback, ethernet, bridge, bond, vlan, wireless, tun or virtual.
	Kind      string    `json:"kind"`
	MAC       string    `json:"mac"`
	MTU       int       `json:"mtu"`
	State     string    `json:"state"`
	Virtual   bool      `json:"virtual"`
	Addresses []Address `json:"addresses"`
}

// Address is an IP address configured on an interface.
type Address struct {
	Address   string `json:"address"`
	PrefixLen int    `json:"prefix_len"`
	Family    string `json:"family"`
	Scope     string `json:"scope"`
}

// DefaultRoute is a default gateway.
type DefaultRoute struct {
	Family    string `json:"family"`
	Interface string `json:"interface"`
	Gateway   string `json:"gateway"`
	Metric    uint32 `json:"metric"`
}

// Normalize sorts the unordered lists for canonical encoding.
func (info *NetworkInfo) Normalize() {
	slices.SortFunc(info.Interfaces, func(a, b Interface) int { return strings.Compare(a.Name, b.Name) })
	for index := range info.Interfaces {
		slices.SortFunc(info.Interfaces[index].Addresses, compareAddresses)
	}
	slices.SortFunc(info.DefaultRoutes, func(a, b DefaultRoute) int {
		if order := strings.Compare(a.Family, b.Family); order != 0 {
			return order
		}
		if a.Metric != b.Metric {
			if a.Metric < b.Metric {
				return -1
			}
			return 1
		}
		if order := strings.Compare(a.Interface, b.Interface); order != 0 {
			return order
		}
		return strings.Compare(a.Gateway, b.Gateway)
	})
}

func compareAddresses(a, b Address) int {
	left, leftErr := netip.ParseAddr(a.Address)
	right, rightErr := netip.ParseAddr(b.Address)
	if leftErr == nil && rightErr == nil {
		if order := left.Compare(right); order != 0 {
			return order
		}
	}
	if order := strings.Compare(a.Address, b.Address); order != 0 {
		return order
	}
	return a.PrefixLen - b.PrefixLen
}

// Network collects the network dataset.
type Network struct {
	host *Host
}

func NewNetwork(host *Host) *Network { return &Network{host: host} }

func (*Network) Name() string { return "network" }

// ephemeralInterfacePrefixes are per-container veth pairs and similar
// interfaces that appear and disappear with workloads.
var ephemeralInterfacePrefixes = []string{"veth", "cali", "lxc", "tap", "vnet", "fwbr", "fwpr", "fwln"}

func (collector *Network) Collect(context.Context) (any, error) {
	host := collector.host
	interfaces := make(map[string]*Interface)

	if entries, err := host.readDir("/sys/class/net"); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if hasAnyPrefix(name, ephemeralInterfacePrefixes) || name == "bonding_masters" {
				continue
			}
			interfaces[name] = host.readSysfsInterface(name)
		}
	}

	if host.Interfaces != nil {
		temporary := host.readTemporaryIPv6()
		live, err := host.Interfaces()
		if err != nil {
			return nil, fmt.Errorf("list interfaces: %w", err)
		}
		for _, entry := range live {
			if hasAnyPrefix(entry.Name, ephemeralInterfacePrefixes) {
				continue
			}
			iface, ok := interfaces[entry.Name]
			if !ok {
				iface = &Interface{Name: entry.Name, Kind: "ethernet", Addresses: []Address{}}
				if entry.Loopback {
					iface.Kind = "loopback"
				}
				interfaces[entry.Name] = iface
			}
			if iface.MAC == "" {
				iface.MAC = entry.MAC
			}
			if iface.MTU == 0 {
				iface.MTU = entry.MTU
			}
			if iface.State == "" {
				iface.State = "down"
				if entry.Up {
					iface.State = "up"
				}
			}
			for _, prefix := range entry.Addresses {
				if temporary[prefix.Addr()] {
					continue
				}
				iface.Addresses = append(iface.Addresses, newAddress(prefix))
			}
		}
	}

	info := &NetworkInfo{Interfaces: []Interface{}, DefaultRoutes: []DefaultRoute{}}
	for _, iface := range interfaces {
		info.Interfaces = append(info.Interfaces, *iface)
	}
	if data, err := host.readFile("/proc/net/route"); err == nil {
		info.DefaultRoutes = append(info.DefaultRoutes, parseIPv4DefaultRoutes(data)...)
	}
	if data, err := host.readFile("/proc/net/ipv6_route"); err == nil {
		info.DefaultRoutes = append(info.DefaultRoutes, parseIPv6DefaultRoutes(data)...)
	}
	info.Normalize()
	return info, nil
}

func newAddress(prefix netip.Prefix) Address {
	address := prefix.Addr().Unmap()
	family := "ipv6"
	if address.Is4() {
		family = "ipv4"
	}
	return Address{
		Address:   address.String(),
		PrefixLen: prefix.Bits(),
		Family:    family,
		Scope:     ClassifyAddress(address),
	}
}

func (host *Host) readSysfsInterface(name string) *Interface {
	base := "/sys/class/net/" + name
	iface := &Interface{
		Name:      name,
		MAC:       strings.ToLower(host.readOptional(base + "/address")),
		State:     host.readOptional(base + "/operstate"),
		Addresses: []Address{},
	}
	if mtu, err := strconv.Atoi(host.readOptional(base + "/mtu")); err == nil {
		iface.MTU = mtu
	}
	if target, err := host.readLink(base); err == nil {
		iface.Virtual = strings.Contains(target, "/virtual/")
	}
	if iface.MAC == "00:00:00:00:00:00" {
		iface.MAC = ""
	}

	linkType := host.readOptional(base + "/type")
	switch {
	case linkType == "772" || name == "lo":
		iface.Kind = "loopback"
		iface.Virtual = true
	case host.isDir(base + "/bridge"):
		iface.Kind = "bridge"
	case host.isDir(base + "/bonding"):
		iface.Kind = "bond"
	case host.isDir(base+"/wireless") || host.isDir(base+"/phy80211"):
		iface.Kind = "wireless"
	case host.exists(base + "/tun_flags"):
		iface.Kind = "tun"
	case host.exists("/proc/net/vlan/" + name):
		iface.Kind = "vlan"
	case linkType == "1" && !iface.Virtual:
		iface.Kind = "ethernet"
	case linkType == "1":
		iface.Kind = "virtual"
	case linkType == "65534":
		iface.Kind = "tun"
	default:
		iface.Kind = "other"
	}
	return iface
}

// ifaTemporary marks RFC 4941 privacy addresses in /proc/net/if_inet6.
const ifaTemporary = 0x01

// readTemporaryIPv6 lists privacy-extension addresses. They rotate daily
// and would otherwise change the dataset hash for no operational reason.
func (host *Host) readTemporaryIPv6() map[netip.Addr]bool {
	temporary := make(map[netip.Addr]bool)
	data, err := host.readFile("/proc/net/if_inet6")
	if err != nil {
		return temporary
	}
	for _, line := range lines(data) {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		flags, err := strconv.ParseUint(fields[4], 16, 32)
		if err != nil || flags&ifaTemporary == 0 {
			continue
		}
		if raw, err := hex.DecodeString(fields[0]); err == nil && len(raw) == 16 {
			temporary[netip.AddrFrom16([16]byte(raw))] = true
		}
	}
	return temporary
}

const (
	routeFlagUp     = 0x0001
	routeFlagReject = 0x0200
)

// parseIPv4DefaultRoutes reads /proc/net/route, whose addresses are
// little-endian hex on every architecture Linux runs servers on.
func parseIPv4DefaultRoutes(data []byte) []DefaultRoute {
	var routes []DefaultRoute
	for index, line := range lines(data) {
		fields := strings.Fields(line)
		if index == 0 || len(fields) < 8 {
			continue
		}
		destination, gateway, mask := fields[1], fields[2], fields[7]
		if destination != "00000000" || mask != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&routeFlagUp == 0 || flags&routeFlagReject != 0 {
			continue
		}
		metric, _ := strconv.ParseUint(fields[6], 10, 32)
		route := DefaultRoute{Family: "ipv4", Interface: fields[0], Metric: uint32(metric)}
		if address, ok := parseLittleEndianIPv4(gateway); ok && !address.IsUnspecified() {
			route.Gateway = address.String()
		}
		routes = append(routes, route)
	}
	return routes
}

func parseLittleEndianIPv4(value string) (netip.Addr, bool) {
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != 4 {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte{raw[3], raw[2], raw[1], raw[0]}), true
}

// parseIPv6DefaultRoutes reads /proc/net/ipv6_route:
// dest dest_len src src_len next_hop metric refcnt use flags iface.
func parseIPv6DefaultRoutes(data []byte) []DefaultRoute {
	var routes []DefaultRoute
	for _, line := range lines(data) {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		if fields[0] != strings.Repeat("0", 32) || fields[1] != "00" {
			continue
		}
		flags, err := strconv.ParseUint(fields[8], 16, 32)
		if err != nil || flags&routeFlagUp == 0 || flags&routeFlagReject != 0 || fields[9] == "lo" {
			continue
		}
		metric, _ := strconv.ParseUint(fields[5], 16, 32)
		route := DefaultRoute{Family: "ipv6", Interface: fields[9], Metric: uint32(metric)}
		if raw, err := hex.DecodeString(fields[4]); err == nil && len(raw) == 16 {
			if address := netip.AddrFrom16([16]byte(raw)); !address.IsUnspecified() {
				route.Gateway = address.String()
			}
		}
		routes = append(routes, route)
	}
	return routes
}
