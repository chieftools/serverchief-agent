package collector

import (
	"context"
	"net/netip"
	"reflect"
	"testing"
	"testing/fstest"
)

func TestClassifyAddress(t *testing.T) {
	tests := map[string]string{
		"8.8.8.8":                  ScopePublic,
		"185.12.64.1":              ScopePublic,
		"10.1.2.3":                 ScopePrivate,
		"172.16.0.1":               ScopePrivate,
		"172.31.255.255":           ScopePrivate,
		"172.32.0.1":               ScopePublic,
		"192.168.1.1":              ScopePrivate,
		"100.64.0.1":               ScopeShared,
		"100.127.255.254":          ScopeShared,
		"100.128.0.1":              ScopePublic,
		"127.0.0.1":                ScopeLoopback,
		"127.10.0.1":               ScopeLoopback,
		"169.254.169.254":          ScopeLinkLocal,
		"224.0.0.1":                ScopeMulticast,
		"0.0.0.0":                  ScopeUnspecified,
		"0.250.250.200":            ScopeReserved,
		"192.0.2.10":               ScopeReserved,
		"198.18.0.1":               ScopeReserved,
		"240.0.0.1":                ScopeReserved,
		"255.255.255.255":          ScopeReserved,
		"2a01:4f8:c0c0:1234::1":    ScopePublic,
		"2606:4700:4700::1111":     ScopePublic,
		"fd07:b51a:cc66:1::2":      ScopePrivate,
		"fc00::1":                  ScopePrivate,
		"fe80::1":                  ScopeLinkLocal,
		"::1":                      ScopeLoopback,
		"::":                       ScopeUnspecified,
		"ff02::1":                  ScopeMulticast,
		"2001:db8::1":              ScopeReserved,
		"::ffff:192.168.1.1":       ScopePrivate,
		"::ffff:100.64.1.1":        ScopeShared,
		"64:ff9b::808:808":         ScopePublic,
		"fec0::1":                  ScopeReserved,
		"100::1":                   ScopeReserved,
		"2002:c000:204::1":         ScopePublic,
		"fe80::9400:1ff:fe23:4567": ScopeLinkLocal,
	}
	for input, want := range tests {
		if got := ClassifyAddress(netip.MustParseAddr(input)); got != want {
			t.Errorf("ClassifyAddress(%s) = %q, want %q", input, got, want)
		}
	}
	if got := ClassifyAddress(netip.Addr{}); got != ScopeUnspecified {
		t.Errorf("zero address = %q", got)
	}
}

func TestParseDefaultRoutes(t *testing.T) {
	ipv4 := parseIPv4DefaultRoutes([]byte("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t01D7A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth1\t00000000\t0100000A\t0003\t0\t0\t200\t00000000\t0\t0\t0\n" +
		"wg0\t00000000\t00000000\t0001\t0\t0\t50\t00000000\t0\t0\t0\n" +
		"eth2\t00000000\t0101A8C0\t0002\t0\t0\t0\t00000000\t0\t0\t0\n" + // not up
		"eth0\t00D7A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"))
	want := []DefaultRoute{
		{Family: "ipv4", Interface: "eth0", Gateway: "192.168.215.1", Metric: 0},
		{Family: "ipv4", Interface: "eth1", Gateway: "10.0.0.1", Metric: 200},
		{Family: "ipv4", Interface: "wg0", Gateway: "", Metric: 50},
	}
	if !reflect.DeepEqual(ipv4, want) {
		t.Errorf("ipv4 = %#v\nwant %#v", ipv4, want)
	}

	ipv6 := parseIPv6DefaultRoutes([]byte(
		"00000000000000000000000000000000 00 00000000000000000000000000000000 00 fd07b51acc6600010000000000000001 00000400 00000001 00000000 00000003     eth0\n" +
			"00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo\n" +
			"fe800000000000000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth0\n"))
	wantIPv6 := []DefaultRoute{{Family: "ipv6", Interface: "eth0", Gateway: "fd07:b51a:cc66:1::1", Metric: 1024}}
	if !reflect.DeepEqual(ipv6, wantIPv6) {
		t.Errorf("ipv6 = %#v\nwant %#v", ipv6, wantIPv6)
	}
}

func prefixes(values ...string) []netip.Prefix {
	result := make([]netip.Prefix, len(values))
	for index, value := range values {
		result[index] = netip.MustParsePrefix(value)
	}
	return result
}

func TestNetworkCollect(t *testing.T) {
	host := fixtureHost(t, "x86-kvm")
	host.Interfaces = func() ([]NetInterface, error) {
		return []NetInterface{
			{Name: "lo", Index: 1, MTU: 65536, Up: true, Loopback: true, Addresses: prefixes("127.0.0.1/8", "::1/128")},
			{Name: "eth0", Index: 2, MTU: 1500, MAC: "96:00:01:23:45:67", Up: true, Addresses: prefixes(
				"2a01:4f8:c0c0:1234::1/64",
				"10.1.1.5/24",
				"2a01:4f8:c0c0:1234:a1b2:c3d4:e5f6:718/64", // temporary, skipped
				"fe80::9400:1ff:fe23:4567/64",
				"203.0.113.7/32",
			)},
			{Name: "wg0", Index: 4, MTU: 1420, Up: true, Addresses: prefixes("100.64.1.2/10")},
			{Name: "veth1a2b3c", Index: 9, MTU: 1500, Up: true},
			{Name: "tailscale0", Index: 7, MTU: 1280, Up: true, Addresses: prefixes("100.101.102.103/32")},
		}, nil
	}
	payload, err := NewNetwork(host).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info := payload.(*NetworkInfo)

	want := []Interface{
		{Name: "docker0", Kind: "bridge", MAC: "02:42:ac:11:00:01", MTU: 1500, State: "down", Virtual: true, Addresses: []Address{}},
		{Name: "eth0", Kind: "ethernet", MAC: "96:00:01:23:45:67", MTU: 1500, State: "up", Virtual: false, Addresses: []Address{
			{Address: "10.1.1.5", PrefixLen: 24, Family: "ipv4", Scope: ScopePrivate},
			{Address: "203.0.113.7", PrefixLen: 32, Family: "ipv4", Scope: ScopeReserved},
			{Address: "2a01:4f8:c0c0:1234::1", PrefixLen: 64, Family: "ipv6", Scope: ScopePublic},
			{Address: "fe80::9400:1ff:fe23:4567", PrefixLen: 64, Family: "ipv6", Scope: ScopeLinkLocal},
		}},
		{Name: "lo", Kind: "loopback", MAC: "", MTU: 65536, State: "unknown", Virtual: true, Addresses: []Address{
			{Address: "127.0.0.1", PrefixLen: 8, Family: "ipv4", Scope: ScopeLoopback},
			{Address: "::1", PrefixLen: 128, Family: "ipv6", Scope: ScopeLoopback},
		}},
		{Name: "tailscale0", Kind: "ethernet", MAC: "", MTU: 1280, State: "up", Addresses: []Address{
			{Address: "100.101.102.103", PrefixLen: 32, Family: "ipv4", Scope: ScopeShared},
		}},
		{Name: "wg0", Kind: "tun", MAC: "", MTU: 1420, State: "unknown", Virtual: true, Addresses: []Address{
			{Address: "100.64.1.2", PrefixLen: 10, Family: "ipv4", Scope: ScopeShared},
		}},
	}
	if !reflect.DeepEqual(info.Interfaces, want) {
		t.Errorf("interfaces =\n%#v\nwant\n%#v", info.Interfaces, want)
	}

	wantRoutes := []DefaultRoute{
		{Family: "ipv4", Interface: "eth0", Gateway: "10.1.1.1", Metric: 100},
		{Family: "ipv6", Interface: "eth0", Gateway: "fe80::1", Metric: 1024},
	}
	if !reflect.DeepEqual(info.DefaultRoutes, wantRoutes) {
		t.Errorf("routes = %#v\nwant %#v", info.DefaultRoutes, wantRoutes)
	}
}

func TestNetworkCollectWithoutSysfs(t *testing.T) {
	host := &Host{FS: fstest.MapFS{}, Interfaces: func() ([]NetInterface, error) {
		return []NetInterface{{Name: "eth0", MTU: 9000, MAC: "aa:bb:cc:dd:ee:ff", Up: false}}, nil
	}}
	payload, err := NewNetwork(host).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info := payload.(*NetworkInfo)
	want := []Interface{{Name: "eth0", Kind: "ethernet", MAC: "aa:bb:cc:dd:ee:ff", MTU: 9000, State: "down", Addresses: []Address{}}}
	if !reflect.DeepEqual(info.Interfaces, want) || len(info.DefaultRoutes) != 0 || info.DefaultRoutes == nil {
		t.Fatalf("info = %#v", info)
	}
}

func TestResolversNetworkManager(t *testing.T) {
	payload, err := NewResolvers(fixtureHost(t, "x86-kvm")).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := &ResolverInfo{
		Nameservers: []string{"10.1.1.53", "fe80::1"},
		Search:      []string{"example.net", "corp.example.net"},
		Options:     []string{"rotate", "timeout:2"},
		ManagedBy:   "NetworkManager",
	}
	if got := payload.(*ResolverInfo); !reflect.DeepEqual(got, want) {
		t.Fatalf("resolvers = %#v\nwant %#v", got, want)
	}
}

func TestResolversSystemdResolvedStub(t *testing.T) {
	payload, err := NewResolvers(fixtureHost(t, "ubuntu-24.04")).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := &ResolverInfo{
		Nameservers: []string{"127.0.0.53"},
		Search:      []string{"example.net"},
		Options:     []string{"edns0", "trust-ad"},
		ManagedBy:   "systemd-resolved",
		SystemdResolved: &SystemdResolved{
			Stub:        "127.0.0.53",
			Nameservers: []string{"1.1.1.1", "9.9.9.9", "2606:4700:4700::1111"},
			Search:      []string{"example.net"},
		},
	}
	if got := payload.(*ResolverInfo); !reflect.DeepEqual(got, want) {
		t.Fatalf("resolvers = %#v\nwant %#v", got, want)
	}
}

func TestResolversResolvconf(t *testing.T) {
	payload, err := NewResolvers(fixtureHost(t, "ubuntu-16.04")).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := payload.(*ResolverInfo)
	if got.ManagedBy != "resolvconf" || got.SystemdResolved != nil || !reflect.DeepEqual(got.Nameservers, []string{"185.12.64.1", "185.12.64.2", "2a01:4ff:ff00::add:1"}) {
		t.Fatalf("resolvers = %#v", got)
	}
}

func TestResolversStubWithoutUpstreamFile(t *testing.T) {
	host := &Host{FS: fstest.MapFS{"etc/resolv.conf": &fstest.MapFile{Data: []byte("nameserver 127.0.0.53\nsearch .\n")}}}
	payload, err := NewResolvers(host).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := payload.(*ResolverInfo)
	if got.SystemdResolved == nil || len(got.SystemdResolved.Nameservers) != 0 || got.SystemdResolved.Nameservers == nil {
		t.Fatalf("systemd_resolved = %#v", got.SystemdResolved)
	}
}

func TestResolversMissingFile(t *testing.T) {
	payload, err := NewResolvers(&Host{FS: fstest.MapFS{}}).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := payload.(*ResolverInfo); len(got.Nameservers) != 0 || got.Nameservers == nil {
		t.Fatalf("resolvers = %#v", got)
	}
}
