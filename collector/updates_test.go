package collector

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func collectUpdates(t *testing.T, host *Host) *UpdatesInfo {
	t.Helper()
	payload, err := NewUpdates(host).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return payload.(*UpdatesInfo)
}

func findUpdate(info *UpdatesInfo, name string) Update {
	for _, item := range info.Items {
		if item.Name == name {
			return item
		}
	}
	return Update{}
}

func TestAptUpdatesUbuntu1604(t *testing.T) {
	host, ran := fakeHost(t, "ubuntu-16.04", map[string]string{
		"var/lib/dpkg/status": "@var/lib/dpkg/status",
		"usr/bin/apt-get":     "",
		"var/lib/apt/lists/archive.ubuntu.com_ubuntu_dists_xenial_InRelease":                  "",
		"var/lib/apt/lists/ports.ubuntu.com_dists_xenial-security_main_binary-arm64_Packages": "",
		"var/run/reboot-required":                   "",
		"var/lib/apt/periodic/update-success-stamp": "",
	}, map[string]fakeCommand{"apt-get -s -q -o Debug::NoLocking=1 dist-upgrade": {file: "apt-get-dist-upgrade.txt"}})
	info := collectUpdates(t, host)

	want := []Update{
		{Name: "base-files", CurrentVersion: "9.4ubuntu4.13", CandidateVersion: "9.4ubuntu4.14", Arch: "arm64"},
		{Name: "libapparmor1", CurrentVersion: "2.10.95-0ubuntu2.11", CandidateVersion: "2.10.95-0ubuntu2.12", Arch: "arm64", Security: true},
	}
	if info.Manager != ManagerApt || !reflect.DeepEqual(info.Items, want) || info.SecurityCount != 1 {
		t.Fatalf("info = %+v", info)
	}
	if info.RebootRequired == nil || !*info.RebootRequired {
		t.Fatal("/var/run/reboot-required not detected")
	}
	if info.CacheAgeSeconds == nil || *info.CacheAgeSeconds != 3600 {
		t.Fatalf("cache age = %v", info.CacheAgeSeconds)
	}
	for _, command := range *ran {
		if strings.Contains(command, "update") && !strings.Contains(command, "dist-upgrade") {
			t.Fatalf("refreshed metadata: %s", command)
		}
	}
}

func TestAptUpdatesDebian12(t *testing.T) {
	files := map[string]string{"var/lib/dpkg/status": "@var/lib/dpkg/status", "bin/apt-get": ""}
	host, _ := fakeHost(t, "debian-12", files, map[string]fakeCommand{"apt-get -s": {file: "apt-get-dist-upgrade-current.txt"}})
	info := collectUpdates(t, host)
	if len(info.Items) != 0 || info.SecurityCount != 0 || info.RebootRequired == nil || *info.RebootRequired || info.CacheAgeSeconds != nil {
		t.Fatalf("up-to-date debian:12 = %+v", info)
	}
	if encoded, _ := json.Marshal(info); string(encoded) != `{"manager":"apt","items":[],"security_count":0,"reboot_required":false,"cache_age_seconds":null}` {
		t.Fatalf("encoded = %s", encoded)
	}

	// debian:12.0 is 45 updates behind the archive.
	host, _ = fakeHost(t, "debian-12", files, map[string]fakeCommand{"apt-get -s": {file: "apt-get-dist-upgrade-12.0.txt"}})
	info = collectUpdates(t, host)
	if len(info.Items) != 45 {
		t.Fatalf("%d updates", len(info.Items))
	}
	if got := findUpdate(info, "perl-base"); got != (Update{Name: "perl-base", CurrentVersion: "5.36.0-7", CandidateVersion: "5.36.0-7+deb12u4", Arch: "arm64", Security: true}) {
		t.Fatalf("perl-base = %+v", got)
	}
	if got := findUpdate(info, "bsdutils"); got.CurrentVersion != "1:2.38.1-5+b1" || got.Security {
		t.Fatalf("bsdutils = %+v", got)
	}
	if info.SecurityCount == 0 || info.SecurityCount == len(info.Items) {
		t.Fatalf("security count = %d", info.SecurityCount)
	}
}

func TestParseAptSimulationVariants(t *testing.T) {
	output := `Inst libc6:i386 [2.23-0ubuntu11] (2.23-0ubuntu11.3 Ubuntu:16.04/xenial-updates [i386]) [libc-bin:amd64 ]
Inst linux-image-4.4.0-210-generic (4.4.0-210.242 Ubuntu:16.04/xenial-security [amd64])
Inst local [1.0] (1.1 [amd64])
Inst tzdata [2024a-0+deb12u1] (2025b-0+deb12u2 Debian:12.13/oldstable-updates, Debian-Security:12/oldstable-security [all])
Conf libc6:i386 (2.23-0ubuntu11.3 Ubuntu:16.04/xenial-updates [i386])
Remv old [1.0]
`
	want := []Update{
		{Name: "libc6", CurrentVersion: "2.23-0ubuntu11", CandidateVersion: "2.23-0ubuntu11.3", Arch: "i386"},
		{Name: "local", CurrentVersion: "1.0", CandidateVersion: "1.1", Arch: "amd64"},
		{Name: "tzdata", CurrentVersion: "2024a-0+deb12u1", CandidateVersion: "2025b-0+deb12u2", Arch: "all", Security: true},
	}
	if got := parseAptSimulation([]byte(output)); !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed = %+v", got)
	}
}

func TestYumUpdatesCentOS7(t *testing.T) {
	host, ran := fakeHost(t, "centos-7", map[string]string{
		"usr/bin/yum":                                "",
		"usr/bin/needs-restarting":                   "",
		"var/lib/rpm/Packages":                       "",
		"var/cache/yum/x86_64/7/base/repomd.xml":     "",
		"var/cache/yum/x86_64/7/updates/cachecookie": "",
	}, map[string]fakeCommand{
		"yum -C -q check-update":             {file: "check-update.txt", exit: 100},
		"yum -C -q updateinfo list security": {file: "updateinfo-security.txt"},
		"rpm -qa":                            {file: "rpm-qa.txt"},
		"/usr/bin/needs-restarting -r":       {file: "needs-restarting.txt"},
	})
	info := collectUpdates(t, host)
	if info.Manager != ManagerYum || len(info.Items) != 51 || info.SecurityCount != 0 {
		t.Fatalf("manager %s, %d items, %d security", info.Manager, len(info.Items), info.SecurityCount)
	}
	if got := findUpdate(info, "bind-license"); got != (Update{Name: "bind-license", CurrentVersion: "32:9.11.4-26.P2.el7", CandidateVersion: "32:9.11.4-26.P2.el7_9.16", Arch: "noarch"}) {
		t.Fatalf("bind-license = %+v", got)
	}
	if info.RebootRequired == nil || *info.RebootRequired {
		t.Fatalf("reboot required = %v", info.RebootRequired)
	}
	if info.CacheAgeSeconds == nil || *info.CacheAgeSeconds != 3600 {
		t.Fatalf("cache age = %v", info.CacheAgeSeconds)
	}
	for _, item := range info.Items {
		if item.CurrentVersion == "" {
			t.Errorf("%s has no current version", item.Name)
		}
	}
	for _, command := range *ran {
		if strings.Contains(command, "makecache") || strings.HasPrefix(command, "yum") && !strings.Contains(command, " -C ") {
			t.Fatalf("may refresh metadata: %s", command)
		}
	}
}

func TestDnfUpdatesAlmaLinux9(t *testing.T) {
	files := map[string]string{
		"usr/bin/dnf":              "",
		"usr/bin/yum":              "",
		"usr/bin/needs-restarting": "",
		"var/lib/rpm/rpmdb.sqlite": "",
		"var/cache/dnf/baseos-09b2e93483836b7c/repodata/repomd.xml": "",
	}
	host, _ := fakeHost(t, "almalinux-9", files, map[string]fakeCommand{
		"dnf -C -q check-update":               {file: "check-update.txt", exit: 100},
		"dnf -C -q updateinfo list --security": {file: "updateinfo-security.txt"},
		"rpm -qa":                              {file: "rpm-qa.txt"},
		"/usr/bin/needs-restarting -r":         {file: "needs-restarting-required.txt", exit: 1},
	})
	info := collectUpdates(t, host)
	if info.Manager != ManagerDnf || len(info.Items) != 13 || info.SecurityCount != 10 {
		t.Fatalf("manager %s, %d items, %d security", info.Manager, len(info.Items), info.SecurityCount)
	}
	if got := findUpdate(info, "openssl-libs"); got != (Update{Name: "openssl-libs", CurrentVersion: "1:3.5.8-1.el9_8", CandidateVersion: "1:3.5.8-2.el9_8", Arch: "aarch64", Security: true}) {
		t.Fatalf("openssl-libs = %+v", got)
	}
	if got := findUpdate(info, "gnutls"); got.Security {
		t.Fatalf("gnutls = %+v", got)
	}
	if info.RebootRequired == nil || !*info.RebootRequired {
		t.Fatal("needs-restarting exit 1 not reported")
	}

	// Without a metadata cache dnf -C fails; the agent reports it rather
	// than refreshing.
	host, ran := fakeHost(t, "almalinux-9", files, map[string]fakeCommand{
		"dnf -C -q check-update": {file: "check-update-nocache.txt", exit: 1},
	})
	_, err := NewUpdates(host).Collect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Cache-only enabled but no cache") || !strings.Contains(err.Error(), "never refreshes") {
		t.Fatalf("no-cache error = %v", err)
	}
	if len(*ran) != 1 {
		t.Fatalf("ran %v", *ran)
	}
}

func TestParseCheckUpdateWrappedAndObsoletes(t *testing.T) {
	output := `
Last metadata expiration check: 0:01:02 ago on Thu Oct 22 12:00:00 2026.
kernel.x86_64                     3.10.0-1160.119.1.el7          updates
a-very-long-package-name-that-wraps.noarch
                                  1.2-3.el7                      epel
Obsoleting Packages
grub2.x86_64                      1:2.02-0.87.el7.centos.14      updates
    grub2.x86_64                  1:2.02-0.86.el7.centos         @anaconda
`
	want := []Update{
		{Name: "kernel", Arch: "x86_64", CandidateVersion: "3.10.0-1160.119.1.el7"},
		{Name: "a-very-long-package-name-that-wraps", Arch: "noarch", CandidateVersion: "1.2-3.el7"},
	}
	if got := parseCheckUpdate([]byte(output)); !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed = %+v", got)
	}
	if name, arch, ok := splitNEVRA("vim-minimal-2:8.2.2637-26.el9_8.23.aarch64"); !ok || name != "vim-minimal" || arch != "aarch64" {
		t.Fatalf("splitNEVRA = %s %s %v", name, arch, ok)
	}
}

func TestUpdatesWithoutManagerOrNeedsRestarting(t *testing.T) {
	host, _ := fakeHost(t, "", nil, nil)
	if _, err := NewUpdates(host).Collect(context.Background()); err == nil {
		t.Fatal("updates without a package manager succeeded")
	}
	host = &Host{FS: fstest.MapFS{}, Now: time.Now}
	if host.needsRestarting(context.Background()) != nil {
		t.Fatal("needs-restarting result without the tool")
	}
}
