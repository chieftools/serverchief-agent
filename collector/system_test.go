package collector

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"testing/fstest"
)

func fixtureHost(t *testing.T, name string) *Host {
	t.Helper()
	root := "testdata/" + name
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return &Host{FS: os.DirFS(root), Root: root}
}

func TestOSReleaseFixtures(t *testing.T) {
	tests := []struct {
		fixture string
		want    OSRelease
	}{
		{
			fixture: "ubuntu-14.04",
			want: OSRelease{
				Family: "debian", ID: "ubuntu", IDLike: []string{"debian"}, Name: "Ubuntu",
				Version: "14.04", VersionFull: "14.04.6", Codename: "trusty",
				PrettyName: "Ubuntu 14.04.6 LTS", Source: "/etc/os-release",
			},
		},
		{
			fixture: "ubuntu-16.04",
			want: OSRelease{
				Family: "debian", ID: "ubuntu", IDLike: []string{"debian"}, Name: "Ubuntu",
				Version: "16.04", VersionFull: "16.04.7", Codename: "xenial",
				PrettyName: "Ubuntu 16.04.7 LTS", Source: "/etc/os-release",
			},
		},
		{
			fixture: "ubuntu-24.04",
			want: OSRelease{
				Family: "debian", ID: "ubuntu", IDLike: []string{"debian"}, Name: "Ubuntu",
				Version: "24.04", VersionFull: "24.04.5", Codename: "noble",
				PrettyName: "Ubuntu 24.04.5 LTS", Source: "/etc/os-release",
			},
		},
		{
			fixture: "debian-8",
			want: OSRelease{
				Family: "debian", ID: "debian", IDLike: []string{}, Name: "Debian GNU/Linux",
				Version: "8", VersionFull: "8.11", Codename: "jessie",
				PrettyName: "Debian GNU/Linux 8 (jessie)", Source: "/etc/os-release",
			},
		},
		{
			fixture: "debian-12",
			want: OSRelease{
				Family: "debian", ID: "debian", IDLike: []string{}, Name: "Debian GNU/Linux",
				Version: "12", VersionFull: "12.7", Codename: "bookworm",
				PrettyName: "Debian GNU/Linux 12 (bookworm)", Source: "/etc/os-release",
			},
		},
		{
			fixture: "centos-7",
			want: OSRelease{
				Family: "rhel", ID: "centos", IDLike: []string{"rhel", "fedora"}, Name: "CentOS Linux",
				Version: "7", VersionFull: "7.9.2009", Codename: "",
				PrettyName: "CentOS Linux 7 (Core)", Source: "/etc/os-release",
			},
		},
		{
			fixture: "almalinux-9",
			want: OSRelease{
				Family: "rhel", ID: "almalinux", IDLike: []string{"rhel", "centos", "fedora"}, Name: "AlmaLinux",
				Version: "9.8", VersionFull: "9.8", Codename: "",
				PrettyName: "AlmaLinux 9.8 (Olive Jaguar)", Source: "/etc/os-release",
			},
		},
		{
			fixture: "centos-6",
			want: OSRelease{
				Family: "rhel", ID: "centos", IDLike: []string{"rhel"}, Name: "CentOS",
				Version: "6", VersionFull: "6.10", Codename: "",
				PrettyName: "CentOS release 6.10 (Final)", Source: "/etc/redhat-release",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.fixture, func(t *testing.T) {
			got := fixtureHost(t, test.fixture).readOSRelease()
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("readOSRelease()\n got %#v\nwant %#v", got, test.want)
			}
		})
	}
}

func TestOSReleaseLSBOnly(t *testing.T) {
	host := &Host{FS: fstest.MapFS{
		"etc/lsb-release": &fstest.MapFile{Data: []byte("DISTRIB_ID=Ubuntu\nDISTRIB_RELEASE=12.04\nDISTRIB_CODENAME=precise\nDISTRIB_DESCRIPTION=\"Ubuntu 12.04.5 LTS\"\n")},
	}}
	got := host.readOSRelease()
	want := OSRelease{
		Family: "debian", ID: "ubuntu", IDLike: []string{"debian"}, Name: "Ubuntu",
		Version: "12.04", VersionFull: "12.04.5", Codename: "precise",
		PrettyName: "Ubuntu 12.04.5 LTS", Source: "/etc/lsb-release",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readOSRelease()\n got %#v\nwant %#v", got, want)
	}
}

func TestOSReleaseUnknown(t *testing.T) {
	got := (&Host{FS: fstest.MapFS{}}).readOSRelease()
	if got.ID != "unknown" || got.Family != "unknown" || got.IDLike == nil {
		t.Fatalf("readOSRelease() = %#v", got)
	}
}

func TestOSFamilyCloudLinux(t *testing.T) {
	release := parseOSRelease(parseKeyValue([]byte(`NAME="CloudLinux"
VERSION="8.10 (Vladimir Aksyonov)"
ID="cloudlinux"
ID_LIKE="rhel fedora centos"
VERSION_ID="8.10"
`)))
	if family := osFamily(release.ID, release.IDLike); family != "rhel" {
		t.Fatalf("family = %q", family)
	}
	if release.VersionFull != "8.10" {
		t.Fatalf("VersionFull = %q", release.VersionFull)
	}
}

func TestTimezone(t *testing.T) {
	tests := map[string]string{
		"ubuntu-14.04": "Europe/Amsterdam", // /etc/timezone
		"ubuntu-24.04": "Europe/Amsterdam", // absolute /etc/localtime symlink
		"almalinux-9":  "UTC",              // relative /etc/localtime symlink
		"centos-7":     "UTC",              // symlink wins over /etc/sysconfig/clock
		"debian-8":     "UTC",              // nothing configured
	}
	for fixture, want := range tests {
		if got := fixtureHost(t, fixture).readTimezone(); got != want {
			t.Errorf("%s: readTimezone() = %q, want %q", fixture, got, want)
		}
	}
	clockOnly := &Host{FS: fstest.MapFS{"etc/sysconfig/clock": &fstest.MapFile{Data: []byte("ZONE=\"America/New_York\"\nUTC=true\n")}}}
	if got := clockOnly.readTimezone(); got != "America/New_York" {
		t.Errorf("sysconfig clock: readTimezone() = %q", got)
	}
}

func TestInitDetectionThroughSystem(t *testing.T) {
	tests := map[string]string{
		"ubuntu-14.04": "upstart",
		"ubuntu-16.04": "systemd",
		"centos-7":     "sysv", // the fixture has no /run/systemd/system: a container
		"debian-8":     "sysv",
		"almalinux-9":  "systemd",
	}
	for fixture, want := range tests {
		payload, err := NewSystem(fixtureHost(t, fixture)).Collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := payload.(SystemInfo).Init; got != want {
			t.Errorf("%s: init = %q, want %q", fixture, got, want)
		}
	}
}

func TestSystemCollectX86(t *testing.T) {
	host := fixtureHost(t, "x86-kvm")
	host.Uname = func() (Uname, error) {
		return Uname{Sysname: "Linux", Release: "5.15.0-124-generic", Version: "#134-Ubuntu SMP", Machine: "x86_64"}, nil
	}
	payload, err := NewSystem(host).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info := payload.(SystemInfo)
	if info.Hostname != "web-01" || info.FQDN != "web-01.example.net" {
		t.Errorf("hostname = %q, fqdn = %q", info.Hostname, info.FQDN)
	}
	if info.Arch != "amd64" || info.Kernel.Machine != "x86_64" || info.Kernel.Release != "5.15.0-124-generic" {
		t.Errorf("arch = %q, kernel = %#v", info.Arch, info.Kernel)
	}
	if info.BootTime != "2026-10-18T05:37:22Z" {
		t.Errorf("boot time = %q", info.BootTime)
	}
	if info.BootID != "6c0a6c39-3e57-4f43-9d0e-1b1c0f8e2b55" {
		t.Errorf("boot id = %q", info.BootID)
	}
	want := Virtualization{Role: "vm", Technology: "kvm", Vendor: "QEMU", Product: "Standard PC (Q35 + ICH9, 2009)"}
	if info.Virtualization != want {
		t.Errorf("virtualization = %#v, want %#v", info.Virtualization, want)
	}
	if info.Init != "systemd" || info.Timezone != "Europe/Amsterdam" {
		t.Errorf("init = %q, timezone = %q", info.Init, info.Timezone)
	}
}

func TestKernelFallsBackToProc(t *testing.T) {
	host := fixtureHost(t, "x86-kvm")
	host.Uname = func() (Uname, error) { return Uname{}, errors.New("no uname") }
	kernel := host.readKernel()
	if kernel.Name != "Linux" || kernel.Release != "5.15.0-124-generic" || kernel.Machine != "" {
		t.Fatalf("readKernel() = %#v", kernel)
	}
}

func TestBootTimeIsPinnedPerBoot(t *testing.T) {
	fsys := fstest.MapFS{
		"proc/stat":                      &fstest.MapFile{Data: []byte("btime 1700000000\n")},
		"proc/sys/kernel/random/boot_id": &fstest.MapFile{Data: []byte("a\n")},
	}
	collector := NewSystem(&Host{FS: fsys})
	first := collector.bootTimeFor("a")
	fsys["proc/stat"] = &fstest.MapFile{Data: []byte("btime 1700000001\n")}
	if again := collector.bootTimeFor("a"); again != first {
		t.Fatalf("boot time moved within one boot: %s -> %s", first, again)
	}
	if rebooted := collector.bootTimeFor("b"); rebooted == first {
		t.Fatalf("boot time not refreshed after reboot")
	}
}

func TestFQDN(t *testing.T) {
	hosts := []byte("127.0.0.1 localhost\n10.0.0.5 db-01.internal.example db-01 # primary\n10.0.0.6 other.example.com db-02\n")
	host := &Host{FS: fstest.MapFS{"etc/hosts": &fstest.MapFile{Data: hosts}}}
	ctx := context.Background()

	if got := host.resolveFQDN(ctx, "db-01"); got != "db-01.internal.example" {
		t.Errorf("hosts entry: %q", got)
	}
	// The canonical name of db-02's line does not extend "db-02".
	if got := host.resolveFQDN(ctx, "db-02"); got != "db-02" {
		t.Errorf("mismatched hosts entry: %q", got)
	}
	if got := host.resolveFQDN(ctx, "Web.Example.COM"); got != "web.example.com" {
		t.Errorf("dotted hostname: %q", got)
	}
	host.LookupCNAME = func(context.Context, string) (string, error) { return "app-01.dc1.example.org.", nil }
	if got := host.resolveFQDN(ctx, "app-01"); got != "app-01.dc1.example.org" {
		t.Errorf("dns lookup: %q", got)
	}
	host.LookupCNAME = func(context.Context, string) (string, error) { return "", errors.New("nxdomain") }
	if got := host.resolveFQDN(ctx, "app-01"); got != "app-01" {
		t.Errorf("failed lookup: %q", got)
	}
}

func TestVirtualizationDetection(t *testing.T) {
	tests := []struct {
		name  string
		fsys  fstest.MapFS
		flags []string
		want  Virtualization
	}{
		{
			name: "nspawn via pid 1 environment",
			fsys: fstest.MapFS{"proc/1/environ": &fstest.MapFile{Data: []byte("PATH=/bin\x00container=systemd-nspawn\x00")}},
			want: Virtualization{Role: "container", Technology: "nspawn"},
		},
		{
			name: "lxc via pid 1 environment wins over dmi",
			fsys: fstest.MapFS{
				"proc/1/environ":              &fstest.MapFile{Data: []byte("container=lxc\x00")},
				"sys/class/dmi/id/sys_vendor": &fstest.MapFile{Data: []byte("Dell Inc.\n")},
			},
			want: Virtualization{Role: "container", Technology: "lxc", Vendor: "Dell Inc."},
		},
		{
			name: "lxc with lxcfs over /proc",
			fsys: fstest.MapFS{
				"proc/1/environ":      &fstest.MapFile{Data: []byte("container=lxc\x00")},
				"proc/self/mountinfo": &fstest.MapFile{Data: []byte("1112 1101 0:50 /proc/cpuinfo /proc/cpuinfo rw,nosuid,nodev,relatime shared:463 - fuse.lxcfs lxcfs rw,user_id=0,group_id=0,allow_other\n1113 1101 0:50 /proc/meminfo /proc/meminfo rw,nosuid,nodev,relatime shared:463 - fuse.lxcfs lxcfs rw,user_id=0,group_id=0,allow_other\n")},
			},
			want: Virtualization{Role: "container", Technology: "lxc", Lxcfs: true},
		},
		{
			name: "openvz",
			fsys: fstest.MapFS{"proc/vz/veinfo": &fstest.MapFile{Data: []byte("")}},
			want: Virtualization{Role: "container", Technology: "openvz"},
		},
		{
			name: "openvz host node is not a container",
			fsys: fstest.MapFS{"proc/vz/veinfo": &fstest.MapFile{}, "proc/bc/0": &fstest.MapFile{}},
			want: Virtualization{Role: "none"},
		},
		{
			name: "docker via cgroup v1",
			fsys: fstest.MapFS{"proc/1/cgroup": &fstest.MapFile{Data: []byte("12:cpuset:/docker/4f3c\n")}},
			want: Virtualization{Role: "container", Technology: "docker"},
		},
		{
			name: "hyper-v",
			fsys: fstest.MapFS{
				"sys/class/dmi/id/sys_vendor":   &fstest.MapFile{Data: []byte("Microsoft Corporation\n")},
				"sys/class/dmi/id/product_name": &fstest.MapFile{Data: []byte("Virtual Machine\n")},
			},
			want: Virtualization{Role: "vm", Technology: "hyperv", Vendor: "Microsoft Corporation", Product: "Virtual Machine"},
		},
		{
			name: "vmware",
			fsys: fstest.MapFS{
				"sys/class/dmi/id/sys_vendor":   &fstest.MapFile{Data: []byte("VMware, Inc.\n")},
				"sys/class/dmi/id/product_name": &fstest.MapFile{Data: []byte("VMware7,1\n")},
			},
			want: Virtualization{Role: "vm", Technology: "vmware", Vendor: "VMware, Inc.", Product: "VMware7,1"},
		},
		{
			name: "hetzner cloud",
			fsys: fstest.MapFS{
				"sys/class/dmi/id/sys_vendor":   &fstest.MapFile{Data: []byte("Hetzner\n")},
				"sys/class/dmi/id/product_name": &fstest.MapFile{Data: []byte("vServer\n")},
			},
			want: Virtualization{Role: "vm", Technology: "kvm", Vendor: "Hetzner", Product: "vServer"},
		},
		{
			name: "xen pv guest without dmi",
			fsys: fstest.MapFS{"proc/xen/capabilities": &fstest.MapFile{Data: []byte("\n")}},
			want: Virtualization{Role: "vm", Technology: "xen"},
		},
		{
			name: "xen dom0 is the hypervisor",
			fsys: fstest.MapFS{"proc/xen/capabilities": &fstest.MapFile{Data: []byte("control_d\n")}},
			want: Virtualization{Role: "none"},
		},
		{
			name:  "unknown hypervisor from cpu flag",
			fsys:  fstest.MapFS{},
			flags: []string{"fpu", "hypervisor"},
			want:  Virtualization{Role: "vm"},
		},
		{
			name: "bare metal",
			fsys: fstest.MapFS{
				"sys/class/dmi/id/sys_vendor":   &fstest.MapFile{Data: []byte("Supermicro\n")},
				"sys/class/dmi/id/product_name": &fstest.MapFile{Data: []byte("SYS-5019C-MR\n")},
			},
			flags: []string{"fpu", "vmx"},
			want:  Virtualization{Role: "none", Vendor: "Supermicro", Product: "SYS-5019C-MR"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := (&Host{FS: test.fsys}).detectVirtualization(test.flags)
			if got != test.want {
				t.Fatalf("detectVirtualization() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestVirtualizationFixtures(t *testing.T) {
	if got := fixtureHost(t, "arm64-neoverse").detectVirtualization(nil); got.Role != "vm" || got.Technology != "amazon" {
		t.Errorf("arm64-neoverse: %#v", got)
	}
	if got := fixtureHost(t, "arm64-apple").detectVirtualization(nil); got.Role != "container" || got.Technology != "docker" {
		t.Errorf("arm64-apple: %#v", got)
	}
}

func TestNormalizeArch(t *testing.T) {
	for machine, want := range map[string]string{"x86_64": "amd64", "aarch64": "arm64", "i686": "386", "armv7l": "arm", "riscv64": "riscv64"} {
		if got := normalizeArch(machine); got != want {
			t.Errorf("normalizeArch(%q) = %q, want %q", machine, got, want)
		}
	}
}

func TestParseKeyValueQuoting(t *testing.T) {
	values := parseKeyValue([]byte("A=\"quoted \\\"x\\\"\"\nB='single'\nexport C=plain\n# comment\nbroken\n"))
	want := map[string]string{"A": `quoted "x"`, "B": "single", "C": "plain"}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("parseKeyValue() = %#v", values)
	}
}
