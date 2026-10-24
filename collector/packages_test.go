package collector

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const packagesTestdata = "testdata/packages"

// fakeCommand is a canned command result read from a fixture file.
type fakeCommand struct {
	file string
	exit int
}

// fakeHost builds a Host whose file system is files plus fixture files and
// whose commands answer from fixtures. Command keys are matched as a prefix
// of "name arg…"; unknown commands fail like a missing binary.
func fakeHost(t *testing.T, distro string, files map[string]string, commands map[string]fakeCommand) (*Host, *[]string) {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, source := range files {
		data := []byte(source)
		if strings.HasPrefix(source, "@") {
			var err error
			data, err = os.ReadFile(filepath.Join(packagesTestdata, distro, source[1:]))
			if err != nil {
				t.Fatal(err)
			}
		}
		fsys[name] = &fstest.MapFile{Data: data, ModTime: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	}
	var ran []string
	host := &Host{
		FS:  fsys,
		Now: func() time.Time { return time.Date(2026, 10, 22, 13, 0, 0, 0, time.UTC) },
		Command: func(_ context.Context, name string, args ...string) (CommandResult, error) {
			line := strings.Join(append([]string{name}, args...), " ")
			ran = append(ran, line)
			for prefix, command := range commands {
				if !strings.HasPrefix(line, prefix) {
					continue
				}
				var output []byte
				if command.file != "" {
					var err error
					output, err = os.ReadFile(filepath.Join(packagesTestdata, distro, "commands", command.file))
					if err != nil {
						t.Fatal(err)
					}
				}
				result := CommandResult{Stdout: output, ExitCode: command.exit}
				if command.exit != 0 && command.exit != 100 {
					result.Stdout, result.Stderr = nil, output
				}
				return result, nil
			}
			return CommandResult{}, os.ErrNotExist
		},
	}
	return host, &ran
}

func collectPackages(t *testing.T, host *Host) *PackagesInfo {
	t.Helper()
	payload, err := NewPackages(host).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return payload.(*PackagesInfo)
}

func findPackage(info *PackagesInfo, name string) Package {
	for _, item := range info.Items {
		if item.Name == name {
			return item
		}
	}
	return Package{}
}

func TestPackagesFromDpkgStatus(t *testing.T) {
	cases := []struct {
		distro string
		count  int
		check  map[string]Package
	}{
		{"ubuntu-16.04", 97, map[string]Package{
			"libc6":      {Name: "libc6", Version: "2.23-0ubuntu11.3", Arch: "arm64", Source: "glibc"},
			"base-files": {Name: "base-files", Version: "9.4ubuntu4.13", Arch: "arm64"},
			// A binary with its own version and epoch.
			"libdevmapper1.02.1": {Name: "libdevmapper1.02.1", Version: "2:1.02.110-1ubuntu10", Arch: "arm64", Source: "lvm2", SourceVersion: "2.02.133-1ubuntu10"},
		}},
		{"debian-12", 88, map[string]Package{
			"libc6": {Name: "libc6", Version: "2.36-9+deb12u14", Arch: "arm64", Source: "glibc"},
			// The binary has an epoch, the source does not.
			"bsdutils": {Name: "bsdutils", Version: "1:2.38.1-5+deb12u3", Arch: "arm64", Source: "util-linux", SourceVersion: "2.38.1-5+deb12u3"},
			// binNMUs: same name, the source version without +bN.
			"bash":        {Name: "bash", Version: "5.2.15-2+b13", Arch: "arm64", SourceVersion: "5.2.15-2"},
			"libcom-err2": {Name: "libcom-err2", Version: "1.47.0-2+b2", Arch: "arm64", Source: "e2fsprogs", SourceVersion: "1.47.0-2"},
		}},
	}
	for _, test := range cases {
		t.Run(test.distro, func(t *testing.T) {
			host, ran := fakeHost(t, test.distro, map[string]string{"var/lib/dpkg/status": "@var/lib/dpkg/status"}, nil)
			info := collectPackages(t, host)
			if info.Manager != ManagerDpkg || len(info.Items) != test.count {
				t.Fatalf("manager %s, %d items", info.Manager, len(info.Items))
			}
			for name, want := range test.check {
				if got := findPackage(info, name); !reflect.DeepEqual(got, want) {
					t.Errorf("%s = %+v, want %+v", name, got, want)
				}
			}
			if !slices.IsSortedFunc(info.Items, func(a, b Package) int {
				return strings.Compare(PackageKey(info.Manager, a), PackageKey(info.Manager, b))
			}) {
				t.Fatal("items are not sorted by delta key")
			}
			if len(*ran) != 0 {
				t.Fatalf("dpkg parsing ran commands: %v", *ran)
			}
		})
	}
}

func TestParseDpkgStatusSkipsUninstalledAndHandlesMultiarch(t *testing.T) {
	status := `Package: removed
Status: deinstall ok config-files
Architecture: amd64
Version: 1.0

Package: held
Status: hold ok installed
Priority: optional
Architecture: amd64
Source: held-src (2.0-1)
Version: 2.0-1
Description: something
 with a continuation line
 .
 and more

Package: libc6
Status: install ok installed
Architecture: amd64
Source: glibc
Version: 2.36-9

Package: libc6
Status: install ok installed
Architecture: i386
Source: glibc
Version: 2.36-9

Package: libfoo1
Status: install ok installed
Architecture: amd64
Source: foo (1:2.0-3)
Version: 1:2.0-3+b1

Package: foo-utils
Status: install ok installed
Architecture: amd64
Source:  foo  ( 2.0-3 )
Version: 2.0-3

Package: half
Status: install ok half-configured
Architecture: amd64
Version: 1
`
	info := &PackagesInfo{Manager: ManagerDpkg, Items: collapsePackages(parseDpkgStatus([]byte(status)), compareDebianVersions)}
	info.Normalize()
	want := []Package{
		{Name: "foo-utils", Version: "2.0-3", Arch: "amd64", Source: "foo"},
		{Name: "held", Version: "2.0-1", Arch: "amd64", Source: "held-src"},
		{Name: "libc6", Version: "2.36-9", Arch: "amd64", Source: "glibc"},
		{Name: "libc6", Version: "2.36-9", Arch: "i386", Source: "glibc"},
		{Name: "libfoo1", Version: "1:2.0-3+b1", Arch: "amd64", Source: "foo", SourceVersion: "1:2.0-3"},
	}
	if !reflect.DeepEqual(info.Items, want) {
		t.Fatalf("items = %+v", info.Items)
	}
}

func TestPackagesFromRPM(t *testing.T) {
	cases := []struct {
		distro   string
		database string
		count    int
		check    map[string]Package
	}{
		{"centos-7", "var/lib/rpm/Packages", 148, map[string]Package{
			"bind-license": {Name: "bind-license", Version: "32:9.11.4-26.P2.el7", Arch: "noarch", Source: "bind"},
			"bash":         {Name: "bash", Version: "4.2.46-34.el7", Arch: "x86_64"},
			// Built from lvm2 with its own version; the epoch is the
			// binary's.
			"device-mapper": {Name: "device-mapper", Version: "7:1.02.170-6.el7", Arch: "x86_64", Source: "lvm2", SourceVersion: "2.02.187-6.el7"},
		}},
		{"almalinux-9", "var/lib/rpm/rpmdb.sqlite", 158, map[string]Package{
			"openssl-libs": {Name: "openssl-libs", Version: "1:3.5.8-1.el9_8", Arch: "aarch64", Source: "openssl"},
			"vim-minimal":  {Name: "vim-minimal", Version: "2:8.2.2637-26.el9_8.21", Arch: "aarch64", Source: "vim"},
		}},
	}
	for _, test := range cases {
		t.Run(test.distro, func(t *testing.T) {
			host, ran := fakeHost(t, test.distro, map[string]string{test.database: "db"},
				map[string]fakeCommand{"rpm -qa --queryformat": {file: "rpm-qa.txt"}})
			info := collectPackages(t, host)
			if info.Manager != ManagerRPM || len(info.Items) != test.count {
				t.Fatalf("manager %s, %d items", info.Manager, len(info.Items))
			}
			for name, want := range test.check {
				if got := findPackage(info, name); !reflect.DeepEqual(got, want) {
					t.Errorf("%s = %+v, want %+v", name, got, want)
				}
			}
			if findPackage(info, "gpg-pubkey").Name != "" {
				t.Fatal("gpg-pubkey pseudo packages must be skipped")
			}
			if want := "rpm -qa --queryformat " + rpmQueryFormat; len(*ran) != 1 || (*ran)[0] != want {
				t.Fatalf("ran %q", *ran)
			}
		})
	}
}

func TestRPMInstallonlyPackagesCollapse(t *testing.T) {
	output := "kernel-core\t(none)\t5.14.0\t503.el9\tx86_64\tkernel-5.14.0-503.el9.src.rpm\n" +
		"kernel-core\t(none)\t5.14.0\t70.13.1.el9_0\tx86_64\tkernel-5.14.0-70.13.1.el9_0.src.rpm\n" +
		"kernel-core\t(none)\t5.14.0\t503.15.1.el9_5\tx86_64\tkernel-5.14.0-503.15.1.el9_5.src.rpm\n" +
		"gpg-pubkey\t(none)\tb86b3716\t61e69f29\t(none)\t(none)\n" +
		"broken line\n"
	items := collapsePackages(parseRPMQuery([]byte(output)), compareRPMVersions)
	want := []Package{{
		Name: "kernel-core", Version: "5.14.0-503.15.1.el9_5", Arch: "x86_64", Source: "kernel",
		Versions: []string{"5.14.0-70.13.1.el9_0", "5.14.0-503.el9", "5.14.0-503.15.1.el9_5"},
	}}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("items = %+v", items)
	}
}

func TestRPMSourceVersion(t *testing.T) {
	output := "openssl-libs\t1\t3.0.7\t27.el9\tx86_64\topenssl-3.0.7-27.el9.src.rpm\n" +
		"perl-Errno\t0\t1.30\t481.el9\tx86_64\tperl-5.32.1-481.el9.src.rpm\n" +
		"perl-libs\t4\t5.32.1\t481.el9\tx86_64\tperl-5.32.1-481.el9.src.rpm\n" +
		"python3-setuptools-wheel\t(none)\t53.0.0\t15.el9\tnoarch\tpython-setuptools-53.0.0-15.el9.src.rpm\n" +
		"kernel-core\t(none)\t5.14.0\t70.13.1.el9_0\tx86_64\tkernel-5.14.0-70.13.1.el9_0.src.rpm\n" +
		"kernel-core\t(none)\t5.14.0\t503.15.1.el9_5\tx86_64\tkernel-5.14.0-503.15.1.el9_5.src.rpm\n" +
		"oddity\t(none)\t1\t1\tx86_64\tbroken.src.rpm\n"
	items := collapsePackages(parseRPMQuery([]byte(output)), compareRPMVersions)
	want := []Package{
		{Name: "openssl-libs", Version: "1:3.0.7-27.el9", Arch: "x86_64", Source: "openssl"},
		{Name: "perl-Errno", Version: "1.30-481.el9", Arch: "x86_64", Source: "perl", SourceVersion: "5.32.1-481.el9"},
		{Name: "perl-libs", Version: "4:5.32.1-481.el9", Arch: "x86_64", Source: "perl"},
		{Name: "python3-setuptools-wheel", Version: "53.0.0-15.el9", Arch: "noarch", Source: "python-setuptools"},
		{Name: "kernel-core", Version: "5.14.0-503.15.1.el9_5", Arch: "x86_64", Source: "kernel", Versions: []string{"5.14.0-70.13.1.el9_0", "5.14.0-503.15.1.el9_5"}},
		{Name: "oddity", Version: "1-1", Arch: "x86_64"},
	}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("items = %+v", items)
	}
	for sourceRPM, want := range map[string][2]string{
		"openssl-3.0.7-27.el9.src.rpm":            {"openssl", "3.0.7-27.el9"},
		"python-setuptools-53.0.0-15.el9.src.rpm": {"python-setuptools", "53.0.0-15.el9"},
		"kernel-5.14.0-503.el9.nosrc.rpm":         {"kernel", "5.14.0-503.el9"},
		"openssl-3.0.7.src.rpm":                   {"", ""},
		"-1-2.src.rpm":                            {"", ""},
		"openssl-3.0.7-.src.rpm":                  {"", ""},
		"(none)":                                  {"", ""},
	} {
		if name, version := rpmSource(sourceRPM); name != want[0] || version != want[1] {
			t.Errorf("rpmSource(%q) = %q, %q", sourceRPM, name, version)
		}
	}
}

func TestCompareRPMVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0-1", "1.0-1", 0},
		{"1.0-1", "1.0-2", -1},
		{"1.10-1", "1.9-1", 1},
		{"1:1.0-1", "2.0-1", 1},
		{"1.0~rc1-1", "1.0-1", -1},
		{"1.0^git1-1", "1.0-1", 1},
		{"1.0a-1", "1.0-1", 1},
		{"1.0-1.el9", "1.0-1.el9_8", -1},
		{"2.34-83.el9", "2.34-283.el9_8", -1},
		{"001-1", "1-1", 0},
	}
	for _, test := range cases {
		if got := compareRPMVersions(test.a, test.b); got != test.want {
			t.Errorf("compare(%q, %q) = %d, want %d", test.a, test.b, got, test.want)
		}
	}
}

func TestPackagesWithoutManager(t *testing.T) {
	host, _ := fakeHost(t, "", nil, nil)
	if _, err := NewPackages(host).Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "no supported package manager") {
		t.Fatalf("err = %v", err)
	}
	host.Command = nil
	host.FS = fstest.MapFS{"var/lib/rpm/Packages": &fstest.MapFile{}}
	if _, err := NewPackages(host).Collect(context.Background()); err == nil {
		t.Fatal("rpm host without commands succeeded")
	}
}

func TestPackagesChangeToken(t *testing.T) {
	fsys := fstest.MapFS{"var/lib/dpkg/status": &fstest.MapFile{Data: []byte("x"), ModTime: time.Unix(100, 0)}}
	packages := NewPackages(&Host{FS: fsys})
	first := packages.ChangeToken()
	fsys["var/lib/dpkg/status"] = &fstest.MapFile{Data: []byte("x"), ModTime: time.Unix(200, 0)}
	if second := packages.ChangeToken(); first == "" || first == second {
		t.Fatalf("tokens %q, %q", first, second)
	}
	if NewUpdates(&Host{FS: fsys}).ChangeToken() == "" {
		t.Fatal("updates has no change token")
	}
}
