package collector

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// All repositories, hosts and keys in these tests are fake (example.test)
// or public distribution mirrors.

func TestParseAptList(t *testing.T) {
	list := `# deb http://commented.example.test/debian bookworm main
deb http://deb.debian.org/debian bookworm main contrib non-free-firmware # trailing comment
deb-src http://deb.debian.org/debian bookworm main
deb [arch=amd64,arm64 signed-by=/usr/share/keyrings/sury.gpg] https://packages.sury.org/php/ bookworm main
deb [ trusted=yes ] http://repo.example.test:8080/some_path/ stable main
deb [trusted=no] https://repo.example.test/flat ./
deb "https://repo.example.test/quoted" bookworm main
deb cdrom:[Debian GNU/Linux 12 _Bookworm_]/ bookworm main
rpm http://apt-rpm.example.test/ stable main
deb http://missing.example.test/suite
deb http://missing.example.test/ bookworm
deb https://repo.example.test/flat ./ main
   deb http://indented.example.test/debian bookworm main

garbage
`
	entries := parseAptList("/etc/apt/sources.list", []byte(list))
	type summary struct {
		kind, uri, suite, components string
		signedBy                     string
		trusted                      bool
		arches                       string
	}
	var got []summary
	for _, entry := range entries {
		signedBy := ""
		if entry.signedBy != nil {
			signedBy = *entry.signedBy
		}
		if !entry.enabled || entry.file != "/etc/apt/sources.list" {
			t.Errorf("entry %+v", entry)
		}
		got = append(got, summary{entry.kind, entry.uri, entry.suite, strings.Join(entry.components, " "), signedBy, entry.trusted, strings.Join(entry.arches, ",")})
	}
	want := []summary{
		{"deb", "http://deb.debian.org/debian", "bookworm", "main contrib non-free-firmware", "", false, ""},
		{"deb-src", "http://deb.debian.org/debian", "bookworm", "main", "", false, ""},
		{"deb", "https://packages.sury.org/php/", "bookworm", "main", "/usr/share/keyrings/sury.gpg", false, "amd64,arm64"},
		{"deb", "http://repo.example.test:8080/some_path/", "stable", "main", "", true, ""},
		{"deb", "https://repo.example.test/flat", "./", "", "", false, ""},
		{"deb", "https://repo.example.test/quoted", "bookworm", "main", "", false, ""},
		{"deb", "cdrom:[Debian GNU/Linux 12 _Bookworm_]/", "bookworm", "main", "", false, ""},
		{"deb", "http://indented.example.test/debian", "bookworm", "main", "", false, ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries:\n%+v\nwant:\n%+v", got, want)
	}
}

// testKey is a throwaway ed25519 key ("Repository Test Key
// <repo@example.test>") generated for these tests; gpg reports the
// fingerprint testKeyFingerprint.
const (
	testKey = `-----BEGIN PGP PUBLIC KEY BLOCK-----

mDMEatzCuxYJKwYBBAHaRw8BAQdAfN8mxWVqoulthEfKL+ETHe2xpk86i0qsWyn8
80uJQfe0J1JlcG9zaXRvcnkgVGVzdCBLZXkgPHJlcG9AZXhhbXBsZS50ZXN0PoiU
BBMWCgA8FiEEPkGTRWcFVrgb4y2bWCi8fPM1F2gFAmrcwrsCGwMFCwkIBwIDIgIB
BhUKCQgLAgQWAgMBAh4HAheAAAoJEFgovHzzNRdo1W4BAJ9aFtsKHxk7qFaIbIvm
wmwbs6S4EqwMJUuE+ODky/1eAP495i3N6S2DRfAtrMIJrdBJd3iDOzCWk2uiSlos
QE5qDw==
=VQ5t
-----END PGP PUBLIC KEY BLOCK-----`
	testKeyFingerprint = "3E419345670556B81BE32D9B5828BC7CF3351768"
)

// deb822Inline indents an armored key as a deb822 continuation, with " ."
// for blank lines.
func deb822Inline(key string) string {
	var out strings.Builder
	for _, line := range strings.Split(key, "\n") {
		if line == "" {
			line = "."
		}
		out.WriteString(" " + line + "\n")
	}
	return out.String()
}

func TestParseDeb822Sources(t *testing.T) {
	sources := `Types: deb deb-src
# http://snapshot.debian.org/archive/debian/20261020T213255Z
URIs: http://deb.debian.org/debian https://mirror.example.test/debian
Suites: bookworm bookworm-updates
Components: main contrib
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

# A disabled stanza.
types: deb
uris: https://disabled.example.test/debian
suites: bookworm
components: main
enabled: no

Types: deb
URIs: https://inline.example.test/apt
Suites: stable
Components: main
Trusted: yes
Architectures: amd64
Signed-By:
` + deb822Inline(testKey) + `
Types: deb
URIs: https://broken-key.example.test/apt
Suites: ./
Signed-By:
 -----BEGIN PGP PUBLIC KEY BLOCK-----
 .
 not base64 at all!
 -----END PGP PUBLIC KEY BLOCK-----

Types: deb
URIs: https://fingerprints.example.test/apt
Suites: noble
Components: main
Signed-By: 0123456789ABCDEF0123456789ABCDEF01234567
 89ABCDEF0123456789ABCDEF0123456789ABCDEF

Types: deb
URIs: https://no-suites.example.test/apt
Components: main

Types: deb
URIs: https://no-components.example.test/apt
Suites: bookworm
`
	entries := parseDeb822Sources("/etc/apt/sources.list.d/test.sources", []byte(sources))
	var got []string
	for _, entry := range entries {
		signedBy := "<nil>"
		if entry.signedBy != nil {
			signedBy = *entry.signedBy
		}
		got = append(got, fmt.Sprintf("%s %s %s [%s] enabled=%v trusted=%v signed-by=%s arch=%s", entry.kind, entry.uri, entry.suite,
			strings.Join(entry.components, " "), entry.enabled, entry.trusted, signedBy, strings.Join(entry.arches, ",")))
	}
	keyring := "signed-by=/usr/share/keyrings/debian-archive-keyring.gpg arch="
	want := []string{
		"deb http://deb.debian.org/debian bookworm [main contrib] enabled=true trusted=false " + keyring,
		"deb http://deb.debian.org/debian bookworm-updates [main contrib] enabled=true trusted=false " + keyring,
		"deb https://mirror.example.test/debian bookworm [main contrib] enabled=true trusted=false " + keyring,
		"deb https://mirror.example.test/debian bookworm-updates [main contrib] enabled=true trusted=false " + keyring,
		"deb-src http://deb.debian.org/debian bookworm [main contrib] enabled=true trusted=false " + keyring,
		"deb-src http://deb.debian.org/debian bookworm-updates [main contrib] enabled=true trusted=false " + keyring,
		"deb-src https://mirror.example.test/debian bookworm [main contrib] enabled=true trusted=false " + keyring,
		"deb-src https://mirror.example.test/debian bookworm-updates [main contrib] enabled=true trusted=false " + keyring,
		"deb https://disabled.example.test/debian bookworm [main] enabled=false trusted=false signed-by=<nil> arch=",
		"deb https://inline.example.test/apt stable [main] enabled=true trusted=true signed-by=" + testKeyFingerprint + " arch=amd64",
		"deb https://broken-key.example.test/apt ./ [] enabled=true trusted=false signed-by=inline arch=",
		"deb https://fingerprints.example.test/apt noble [main] enabled=true trusted=false signed-by=0123456789ABCDEF0123456789ABCDEF01234567 89ABCDEF0123456789ABCDEF0123456789ABCDEF arch=",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, entry := range entries {
		if entry.signedBy != nil && strings.Contains(*entry.signedBy, "BEGIN PGP") {
			t.Fatal("an inline key is reported whole")
		}
	}
}

func TestArmoredFingerprints(t *testing.T) {
	if got := armoredFingerprints(testKey); !reflect.DeepEqual(got, []string{testKeyFingerprint}) {
		t.Fatalf("fingerprints = %v", got)
	}
	withHeaders := strings.Replace(testKey, "BLOCK-----\n\n", "BLOCK-----\nComment: test\nVersion: GnuPG\n\n", 1)
	if got := armoredFingerprints(withHeaders); !reflect.DeepEqual(got, []string{testKeyFingerprint}) {
		t.Fatalf("with armor headers = %v", got)
	}
	for _, broken := range []string{"", "-----BEGIN PGP PUBLIC KEY BLOCK-----\nAAAA\n", strings.Replace(testKey, "mDME", "!!!!", 1)} {
		if got := armoredFingerprints(broken); got != nil {
			t.Errorf("%q = %v", broken, got)
		}
	}
}

func TestSanitizeURI(t *testing.T) {
	for uri, want := range map[string]string{
		"https://deb.debian.org/debian":                                    "https://deb.debian.org/debian",
		"https://user:s3cret@repo.example.test/debian":                     "https://repo.example.test/debian",
		"https://token@repo.example.test:8443/debian/":                     "https://repo.example.test:8443/debian/",
		"mirror+https://user:pw@mirrors.example.test/list.txt":             "mirror+https://mirrors.example.test/list.txt",
		"https://repo.example.test/el9/$basearch/?token=abc123&arch=x":     "https://repo.example.test/el9/$basearch/",
		"https://mirrors.example.test/metalink?repo=epel-9&arch=$basearch": "https://mirrors.example.test/metalink",
		"https://repo.example.test/debian#fragment":                        "https://repo.example.test/debian",
		"https://repo.example.test/path@with/at":                           "https://repo.example.test/path@with/at",
		"https://a:b@c@repo.example.test/":                                 "https://repo.example.test/",
		"file:/srv/repo":                                                   "file:/srv/repo",
		"file:///etc/pki/rpm-gpg/RPM-GPG-KEY-EPEL-9":                       "file:///etc/pki/rpm-gpg/RPM-GPG-KEY-EPEL-9",
		"cdrom:[Debian GNU/Linux 12 _Bookworm_]/":                          "cdrom:[Debian GNU/Linux 12 _Bookworm_]/",
		"http://[2001:db8::1]:8080/debian":                                 "http://[2001:db8::1]:8080/debian",
		"http://user:pw@[2001:db8::1]/debian":                              "http://[2001:db8::1]/debian",
	} {
		if got := sanitizeURI(uri); got != want {
			t.Errorf("sanitizeURI(%q) = %q, want %q", uri, got, want)
		}
	}
}

func TestSanitizeMirrorURI(t *testing.T) {
	for uri, want := range map[string]string{
		// The identifying parameters stay, as written and in order.
		"https://mirrors.fedoraproject.org/metalink?repo=epel-$releasever&arch=$basearch&infra=$infra&content=$contentdir": "https://mirrors.fedoraproject.org/metalink?repo=epel-$releasever&arch=$basearch&infra=$infra&content=$contentdir",
		"http://mirrorlist.centos.org/?release=$releasever&arch=$basearch&repo=os&infra=$infra":                            "http://mirrorlist.centos.org/?release=$releasever&arch=$basearch&repo=os&infra=$infra",
		"https://mirrors.example.test/list?country=NL&protocol=https&repo=remi-9":                                          "https://mirrors.example.test/list?country=NL&protocol=https&repo=remi-9",
		// A token-like parameter on the same URL is dropped, as is userinfo.
		"https://ci:s3cret@mirrors.example.test/metalink?repo=private-9&token=hunter2&arch=x86_64&key=abc#frag": "https://mirrors.example.test/metalink?repo=private-9&arch=x86_64",
		"https://mirrors.example.test/metalink?token=hunter2&REPO=upper":                                        "https://mirrors.example.test/metalink",
		"https://mirrors.almalinux.org/mirrorlist/$releasever/baseos":                                           "https://mirrors.almalinux.org/mirrorlist/$releasever/baseos",
	} {
		if got := sanitizeMirrorURI(uri); got != want {
			t.Errorf("sanitizeMirrorURI(%q) =\n %q, want\n %q", uri, got, want)
		}
	}
	// Elsewhere the whole query goes, identifying parameters included.
	if got := sanitizeURI("https://repo.example.test/el9/?repo=epel-9&arch=x86_64"); got != "https://repo.example.test/el9/" {
		t.Errorf("baseurl query kept: %q", got)
	}
}

func TestURITransport(t *testing.T) {
	for uri, want := range map[string]string{
		"https://deb.debian.org/debian":          "https",
		"HTTP://deb.debian.org/debian":           "http",
		"file:/srv/repo":                         "file",
		"cdrom:[Debian GNU/Linux 12]/":           "cdrom",
		"mirror+https://mirrors.example.test/l":  "mirror",
		"mirror+file:/etc/apt/mirrors.txt":       "mirror",
		"tor+https://onion.example.test/debian":  "tor",
		"ftp://ftp.example.test/debian":          "ftp",
		"/no/scheme":                             "",
		"[2001:db8::1]/x":                        "",
		"https://mirrors.example.test/$basearch": "https",
	} {
		if got := uriTransport(uri); got != want {
			t.Errorf("uriTransport(%q) = %q, want %q", uri, got, want)
		}
	}
}

// TestAptListNames pins apt's URItoFileName, from `apt-get --print-uris
// update` on debian:12 (apt 2.6).
func TestAptListNames(t *testing.T) {
	for uri, want := range map[string]string{
		"http://deb.debian.org/debian/dists/bookworm/main/binary-amd64/Packages":                      "deb.debian.org_debian_dists_bookworm_main_binary-amd64_Packages",
		"http://user:secret@repo.example.test:8080/some_path/deb~x/dists/bookworm/main/binary-arm64/": "repo.example.test:8080_some%5fpath_deb%7ex_dists_bookworm_main_binary-arm64_",
		"https://repo.example.test/q?token=abc/dists/bookworm/main/binary-all/Packages":               "repo.example.test_q?token%3dabc_dists_bookworm_main_binary-all_Packages",
		"http://[2001:db8::1]/debian/dists/bookworm/main/binary-arm64/Packages":                       "2001:db8::1_debian_dists_bookworm_main_binary-arm64_Packages",
		"file:/srv/local-repo/Packages":                                                                  "_srv_local-repo_Packages",
		"https://repo.example.test/flat/./Packages":                                                      "repo.example.test_flat_._Packages",
		"mirror+file:/etc/apt/mirrors.txt/dists/bookworm/main/binary-arm64/Packages":                     "_etc_apt_mirrors.txt_dists_bookworm_main_binary-arm64_Packages",
		"mirror+http://mirrors.example.test/list.txt/dists/bookworm/InRelease":                           "mirrors.example.test_list.txt_dists_bookworm_InRelease",
		"cdrom:[Debian GNU/Linux 12 _Bookworm_]/dists/bookworm/main/binary-arm64/Packages":               "Debian%20GNU_Linux%2012%20%5fBookworm%5f_dists_bookworm_main_binary-arm64_Packages",
		"http://repo.example.test:8080/some_path/deb~x/dists/bookworm/contrib/extra/binary-all/Packages": "repo.example.test:8080_some%5fpath_deb%7ex_dists_bookworm_contrib_extra_binary-all_Packages",
	} {
		if got := aptListName(uri); got != want {
			t.Errorf("aptListName(%q) =\n %q, want\n %q", uri, got, want)
		}
	}
}

func TestAptIndexMatcher(t *testing.T) {
	entry := func(uri, suite string, components ...string) aptEntry {
		return aptEntry{kind: RepositoryDeb, uri: uri, suite: suite, components: components, enabled: true}
	}
	for _, test := range []struct {
		entry aptEntry
		name  string
		want  bool
	}{
		{entry("http://deb.debian.org/debian", "bookworm", "main"), "deb.debian.org_debian_dists_bookworm_main_binary-amd64_Packages", true},
		{entry("http://deb.debian.org/debian/", "bookworm", "main"), "deb.debian.org_debian_dists_bookworm_main_binary-all_Packages", true},
		{entry("http://deb.debian.org/debian", "bookworm", "contrib", "main"), "deb.debian.org_debian_dists_bookworm_main_binary-arm64_Packages", true},
		{entry("http://deb.debian.org/debian", "bookworm", "contrib"), "deb.debian.org_debian_dists_bookworm_main_binary-arm64_Packages", false},
		// The suite must match exactly, not as a prefix.
		{entry("http://deb.debian.org/debian", "bookworm", "main"), "deb.debian.org_debian_dists_bookworm-updates_main_binary-amd64_Packages", false},
		{entry("http://deb.debian.org/debian", "bookworm-updates", "main"), "deb.debian.org_debian_dists_bookworm-updates_main_binary-amd64_Packages", true},
		{entry("http://deb.debian.org/debian-security", "bookworm-security", "main"), "deb.debian.org_debian-security_dists_bookworm-security_main_binary-amd64_Packages", true},
		{entry("http://deb.debian.org/debian", "bookworm", "main"), "deb.debian.org_debian-security_dists_bookworm-security_main_binary-amd64_Packages", false},
		// Ports, paths, credentials (never in the file name) and quoting.
		{entry("https://user:pw@repo.example.test:8443/some_path/apt", "stable", "main"), "repo.example.test:8443_some%5fpath_apt_dists_stable_main_binary-amd64_Packages", true},
		{entry("https://repo.example.test/some_path/apt", "stable", "main"), "repo.example.test:8443_some%5fpath_apt_dists_stable_main_binary-amd64_Packages", false},
		{entry("http://ports.ubuntu.com/ubuntu-ports/", "xenial-security", "universe"), "ports.ubuntu.com_ubuntu-ports_dists_xenial-security_universe_binary-arm64_Packages", true},
		// Components with a slash, and suites with one.
		{entry("http://repo.example.test/debian", "stable/updates", "main"), "repo.example.test_debian_dists_stable_updates_main_binary-amd64_Packages", true},
		{entry("http://repo.example.test/debian", "bookworm", "contrib/extra"), "repo.example.test_debian_dists_bookworm_contrib_extra_binary-amd64_Packages", true},
		// binary-<arch> must be one word before _Packages.
		{entry("http://repo.example.test/debian", "bookworm", "main"), "repo.example.test_debian_dists_bookworm_main_binary-amd64_i18n_Packages", false},
		{entry("http://repo.example.test/debian", "bookworm", "main"), "repo.example.test_debian_dists_bookworm_main_binary-_Packages", false},
		// Flat repositories.
		{entry("https://repo.example.test/flat", "./"), "repo.example.test_flat_._Packages", true},
		{entry("file:/srv/local-repo", "/"), "_srv_local-repo_Packages", true},
		{entry("https://repo.example.test/sub/", "dist/with/slash/"), "repo.example.test_sub_dist_with_slash_Packages", true},
		{entry("https://repo.example.test/flat", "./"), "repo.example.test_flat_dists_._main_binary-amd64_Packages", false},
	} {
		if got := newAptIndexMatcher(test.entry).matches(test.name); got != test.want {
			t.Errorf("%s %s %v matches %s = %v", test.entry.uri, test.entry.suite, test.entry.components, test.name, got)
		}
	}
}

// lz4Stored wraps data in an lz4 frame of uncompressed blocks.
func lz4Stored(data []byte) []byte {
	frame := []byte{0x04, 0x22, 0x4d, 0x18, 0x60, 0x40, 0x82}
	for len(data) > 0 {
		size := min(len(data), 64<<10)
		frame = binary.LittleEndian.AppendUint32(frame, uint32(size)|0x80000000)
		frame = append(frame, data[:size]...)
		data = data[size:]
	}
	return binary.LittleEndian.AppendUint32(frame, 0)
}

func gzipped(t *testing.T, data string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestLZ4Reader(t *testing.T) {
	read := func(data []byte) (string, error) {
		out, err := io.ReadAll(newLZ4Reader(bytes.NewReader(data)))
		return string(out), err
	}
	// Uncompressed blocks, two concatenated frames, a skippable frame.
	skippable := []byte{0x50, 0x2a, 0x4d, 0x18, 3, 0, 0, 0, 'x', 'y', 'z'}
	stream := slices.Concat(lz4Stored([]byte("Package: a\n")), skippable, lz4Stored([]byte("Package: b\n")))
	if got, err := read(stream); err != nil || got != "Package: a\nPackage: b\n" {
		t.Fatalf("stored frames = %q, %v", got, err)
	}
	// A compressed block: literal "ab", then an overlapping match of
	// length 6 at offset 2, then the literal "c".
	block := []byte{0x22, 'a', 'b', 0x02, 0x00, 0x10, 'c'}
	frame := slices.Concat([]byte{0x04, 0x22, 0x4d, 0x18, 0x60, 0x40, 0x82}, binary.LittleEndian.AppendUint32(nil, uint32(len(block))), block, []byte{0, 0, 0, 0})
	if got, err := read(frame); err != nil || got != "abababab"+"c" {
		t.Fatalf("compressed block = %q, %v", got, err)
	}
	for name, broken := range map[string][]byte{
		"bad magic":     {1, 2, 3, 4, 5, 6, 7},
		"truncated":     frame[:len(frame)-6],
		"bad offset":    slices.Concat(frame[:11], []byte{0x22, 'a', 'b', 0x09, 0x00, 0x10, 'c'}, []byte{0, 0, 0, 0}),
		"no end mark":   frame[:len(frame)-4],
		"huge block":    slices.Concat(frame[:7], []byte{0xff, 0xff, 0xff, 0x7f}),
		"reserved flag": {0x04, 0x22, 0x4d, 0x18, 0x62, 0x40, 0x82, 0, 0, 0, 0},
	} {
		if _, err := read(broken); err == nil {
			t.Errorf("%s: no error", name)
		}
	}

	// testdata/repositories/synthetic_Packages.lz4 is `lz4 -9 -BD -B4
	// --content-size` (lz4 1.10) of syntheticPackages: linked 64 KiB
	// blocks whose matches reach into the previous block.
	compressed, err := os.ReadFile("testdata/repositories/synthetic_Packages.lz4")
	if err != nil {
		t.Fatal(err)
	}
	got, err := read(compressed)
	if err != nil || got != syntheticPackages() {
		t.Fatalf("lz4 CLI fixture: %d bytes, %v (want %d bytes)", len(got), err, len(syntheticPackages()))
	}
}

func syntheticPackages() string {
	var out strings.Builder
	for index := range 3000 {
		arch := "amd64"
		if index%5 == 0 {
			arch = "all"
		}
		fmt.Fprintf(&out, "Package: synthetic-%d\nVersion: 1.%d-%d\nArchitecture: %s\nMaintainer: Test <repo@example.test>\nDescription: synthetic package %d\n\n", index, index%7, index, arch, index)
	}
	return out.String()
}

// recordingFS records every name opened, to prove auth files stay unread.
type recordingFS struct {
	fs.FS
	mu     sync.Mutex
	opened []string
}

func (recording *recordingFS) Open(name string) (fs.File, error) {
	recording.mu.Lock()
	recording.opened = append(recording.opened, name)
	recording.mu.Unlock()
	return recording.FS.Open(name)
}

// aptOriginsHost is a Debian 12 host with the Sury repository, a manually
// installed package, a superseded version and a list of a removed
// repository.
func aptOriginsHost(t *testing.T) (fstest.MapFS, *recordingFS, *Host) {
	t.Helper()
	status := `Package: coreutils
Status: install ok installed
Architecture: amd64
Version: 9.1-1

Package: php8.3-cli
Status: install ok installed
Architecture: amd64
Source: php8.3
Version: 8.3.12-1+0~20261001.1+debian12~1.gbp1a2b3c

Package: tzdata
Status: install ok installed
Architecture: all
Version: 2026a-0+deb12u1

Package: local-tool
Status: install ok installed
Architecture: amd64
Version: 0.1

Package: openssl
Status: install ok installed
Architecture: amd64
Version: 3.0.11-1~deb12u1

Package: removed
Status: deinstall ok config-files
Architecture: amd64
Version: 9.1-1
`
	debianMain := `Package: coreutils
Version: 9.1-1
Architecture: amd64
Description: GNU core utilities
 with a continuation line
 Package: not-a-field

Package: coreutils
Version: 9.1-1
Architecture: arm64

Package: tzdata
Architecture: all
Version: 2026a-0+deb12u1

Package: openssl
Version: 3.0.15-1~deb12u1
Architecture: amd64

Package: removed
Version: 9.1-1
Architecture: amd64
`
	updates := "Package: tzdata\r\nVersion: 2026a-0+deb12u1\r\nArchitecture: all\r\n"
	security := "Package: openssl\nVersion: 3.0.16-1~deb12u1\nArchitecture: amd64\n\n"
	sury := "Package: php8.3-cli\nVersion: 8.3.12-1+0~20261001.1+debian12~1.gbp1a2b3c\nArchitecture: amd64\n\nPackage: coreutils\nVersion: 9.4-1+sury\nArchitecture: amd64\n"
	debianSources := `Types: deb
URIs: http://deb.debian.org/debian
Suites: bookworm bookworm-updates
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

Types: deb
URIs: http://deb.debian.org/debian-security
Suites: bookworm-security
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
`
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	file := func(data string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(data), ModTime: at} }
	lists := "var/lib/apt/lists/"
	fsys := fstest.MapFS{
		"usr/bin/apt-get":     file(""),
		"var/lib/dpkg/status": file(status),
		"etc/apt/sources.list": file("# See sources.list.d\n" +
			"deb [signed-by=/etc/apt/keyrings/private.gpg] https://ci:s3cret-token@private.example.test:8443/apt/?key=hunter2 bookworm main\n"),
		"etc/apt/sources.list.d/debian.sources": file(debianSources),
		"etc/apt/sources.list.d/php.list":       file("deb [signed-by=/usr/share/keyrings/deb.sury.org-php.gpg] https://packages.sury.org/php/ bookworm main\n"),
		"etc/apt/sources.list.d/php.list.save":  file("deb https://stale.example.test/php/ bookworm main\n"),
		"etc/apt/auth.conf":                     file("machine private.example.test login ci password s3cret-auth\n"),
		"etc/apt/auth.conf.d/vendor.conf":       file("machine vendor.example.test login x password s3cret-vendor\n"),

		lists + "deb.debian.org_debian_dists_bookworm_InRelease":                                  {Data: []byte("x"), ModTime: at},
		lists + "deb.debian.org_debian_dists_bookworm_main_binary-amd64_Packages":                 file(debianMain),
		lists + "deb.debian.org_debian_dists_bookworm-updates_main_binary-amd64_Packages.gz":      {Data: gzipped(t, updates), ModTime: at},
		lists + "deb.debian.org_debian-security_dists_bookworm-security_main_binary-all_Packages": file(security),
		lists + "packages.sury.org_php_dists_bookworm_main_binary-amd64_Packages.lz4":             {Data: lz4Stored([]byte(sury)), ModTime: at},
		lists + "old.example.test_debian_dists_bookworm_main_binary-amd64_Packages":               file("Package: local-tool\nVersion: 0.1\nArchitecture: amd64\n"),
		lists + "lock": file(""),
	}
	recording := &recordingFS{FS: fsys}
	return fsys, recording, &Host{FS: recording}
}

func findRepository(t *testing.T, info *RepositoriesInfo, uri, suite string) Repository {
	t.Helper()
	for _, item := range info.Items {
		if item.Type == RepositoryDeb && item.URIs[0] == uri && item.Suite == suite || item.Type == RepositoryRPM && item.ID == suite {
			return item
		}
	}
	t.Fatalf("no repository %s %s in %+v", uri, suite, info.Items)
	return Repository{}
}

func TestAptRepositoriesAndOrigins(t *testing.T) {
	fsys, recording, host := aptOriginsHost(t)
	origins := NewOrigins(host)
	payload, err := NewRepositories(host, origins).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	repositories := payload.(*RepositoriesInfo)
	if repositories.Manager != ManagerApt || len(repositories.Items) != 5 {
		t.Fatalf("repositories = %+v", repositories)
	}
	main := findRepository(t, repositories, "http://deb.debian.org/debian", "bookworm")
	updates := findRepository(t, repositories, "http://deb.debian.org/debian", "bookworm-updates")
	security := findRepository(t, repositories, "http://deb.debian.org/debian-security", "bookworm-security")
	sury := findRepository(t, repositories, "https://packages.sury.org/php/", "bookworm")
	private := findRepository(t, repositories, "https://private.example.test:8443/apt/", "bookworm")

	// Configuration order: sources.list, then sources.list.d by name.
	var order []string
	for _, item := range repositories.Items {
		order = append(order, item.ID)
	}
	if !reflect.DeepEqual(order, []string{private.ID, main.ID, updates.ID, security.ID, sury.ID}) {
		t.Errorf("order = %v", order)
	}
	// Ids are stable: pinned, so a refactor cannot silently change them.
	if main.ID != "bookworm:09c4df46" {
		t.Errorf("bookworm id = %s", main.ID)
	}
	keyring := "/usr/share/keyrings/debian-archive-keyring.gpg"
	wantMain := Repository{
		ID: main.ID, File: "/etc/apt/sources.list.d/debian.sources", Type: "deb", Enabled: true,
		URIs: []string{"http://deb.debian.org/debian"}, Suite: "bookworm", Components: []string{"main"},
		Signing: RepositorySigning{SignedBy: &keyring}, Transport: "http", Packages: 2,
	}
	if !reflect.DeepEqual(main, wantMain) {
		t.Errorf("bookworm = %+v", main)
	}
	for _, check := range []struct {
		repository Repository
		packages   int
	}{{updates, 1}, {security, 0}, {sury, 1}, {private, 0}} {
		if check.repository.Packages != check.packages {
			t.Errorf("%s has %d packages, want %d", check.repository.ID, check.repository.Packages, check.packages)
		}
		if !strings.HasPrefix(check.repository.ID, check.repository.Suite+":") || len(check.repository.ID) != len(check.repository.Suite)+9 {
			t.Errorf("id %q", check.repository.ID)
		}
	}
	if private.Transport != "https" || private.File != "/etc/apt/sources.list" {
		t.Errorf("private = %+v", private)
	}

	packages := collectPackagesWith(t, NewPackagesWithOrigins(host, origins))
	want := map[string][]string{
		"coreutils":  {main.ID},
		"tzdata":     sortedIDs(main.ID, updates.ID),
		"php8.3-cli": {sury.ID},
		"local-tool": {}, // from a removed repository, or a manually installed .deb
		"openssl":    {}, // the installed version is superseded and gone
	}
	for name, ids := range want {
		if got := findPackage(packages, name).Origins; !reflect.DeepEqual(got, ids) {
			t.Errorf("%s origins = %#v, want %#v", name, got, ids)
		}
	}
	wire, _ := json.Marshal(findPackage(packages, "openssl"))
	if !strings.Contains(string(wire), `"origins":[]`) {
		t.Errorf("an empty origins list is left out: %s", wire)
	}

	// Credentials: nothing secret is reported, and auth files are never
	// opened.
	wire, _ = json.Marshal(repositories)
	for _, secret := range []string{"s3cret", "hunter2", "key=", "ci:", "stale.example.test"} {
		if strings.Contains(string(wire), secret) {
			t.Errorf("payload contains %q: %s", secret, wire)
		}
	}
	for _, name := range recording.opened {
		if strings.Contains(name, "auth.conf") {
			t.Errorf("opened %s", name)
		}
	}

	// The index is reused until something changes.
	opened := len(recording.opened)
	if _, err := NewRepositories(host, origins).Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range recording.opened[opened:] {
		if strings.Contains(name, "_Packages") {
			t.Fatalf("lists read again without a change: %s", name)
		}
	}
	fsys["var/lib/dpkg/status"].ModTime = time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)
	opened = len(recording.opened)
	_ = collectPackagesWith(t, NewPackagesWithOrigins(host, origins))
	if !slices.ContainsFunc(recording.opened[opened:], func(name string) bool { return strings.HasSuffix(name, "_Packages") }) {
		t.Fatal("a changed package database did not re-read the lists")
	}
}

func sortedIDs(ids ...string) []string {
	slices.Sort(ids)
	return ids
}

func collectPackagesWith(t *testing.T, collector *Packages) *PackagesInfo {
	t.Helper()
	payload, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return payload.(*PackagesInfo)
}

func TestAptOriginsUndetermined(t *testing.T) {
	// Before the first apt-get update there are no lists: origins are left
	// out and repositories count nothing.
	fsys, _, host := aptOriginsHost(t)
	for name := range fsys {
		if strings.Contains(name, "_Packages") {
			delete(fsys, name)
		}
	}
	packages := collectPackages(t, host)
	wire, _ := json.Marshal(packages)
	if strings.Contains(string(wire), "origins") {
		t.Fatalf("origins without lists: %s", wire)
	}
	payload, err := NewRepositories(host, nil).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range payload.(*RepositoriesInfo).Items {
		if item.Packages != 0 {
			t.Errorf("%s counts %d packages", item.ID, item.Packages)
		}
	}

	// A list in a format the agent cannot read leaves them out too, rather
	// than reporting its packages as from nowhere.
	fsys, _, host = aptOriginsHost(t)
	fsys["var/lib/apt/lists/packages.sury.org_php_dists_bookworm_main_binary-amd64_Packages.xz"] = fsys["var/lib/apt/lists/packages.sury.org_php_dists_bookworm_main_binary-amd64_Packages.lz4"]
	delete(fsys, "var/lib/apt/lists/packages.sury.org_php_dists_bookworm_main_binary-amd64_Packages.lz4")
	if wire, _ := json.Marshal(collectPackages(t, host)); strings.Contains(string(wire), "origins") {
		t.Fatalf("origins with an unreadable list: %s", wire)
	}
	// A corrupt one as well.
	fsys, _, host = aptOriginsHost(t)
	fsys["var/lib/apt/lists/deb.debian.org_debian_dists_bookworm-updates_main_binary-amd64_Packages.gz"].Data = []byte("not gzip")
	if wire, _ := json.Marshal(collectPackages(t, host)); strings.Contains(string(wire), "origins") {
		t.Fatalf("origins with a corrupt list: %s", wire)
	}
}

// rpmRepoFiles are .repo files with credentials that must not be reported.
var rpmRepoFiles = map[string]string{
	"etc/dnf/dnf.conf": "[main]\ngpgcheck=1\nproxy_password=s3cret-proxy\n",
	"etc/yum.repos.d/almalinux-baseos.repo": `[baseos]
name=AlmaLinux $releasever - BaseOS
mirrorlist=https://mirrors.almalinux.org/mirrorlist/$releasever/baseos
# baseurl=https://repo.almalinux.org/almalinux/$releasever/BaseOS/$basearch/os/
enabled=1
gpgcheck=1
countme=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-AlmaLinux-9

[baseos-debuginfo]
name=AlmaLinux $releasever - BaseOS - Debug
mirrorlist=https://mirrors.almalinux.org/mirrorlist/$releasever/baseos-debuginfo
enabled=0
`,
	"etc/yum.repos.d/epel.repo": `[epel]
name=Extra Packages for Enterprise Linux $releasever - $basearch
metalink=https://mirrors.fedoraproject.org/metalink?repo=epel-$releasever&arch=$basearch&infra=$infra&content=$contentdir
enabled=1
gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-EPEL-$releasever
`,
	"etc/yum.repos.d/private.repo": `; vendor repository
[private]
name = Private repository
baseurl = https://ci:s3cret-base@private.example.test/el9/$basearch/?token=hunter2&repo=private
          https://mirror.example.test/el9/$basearch/
username=ci
password=s3cret-password
sslclientkey=/etc/pki/private.key
Enabled = 0
gpgcheck = no

[main]
gpgcheck=0

[private-mirror]
name=Private mirror list
mirrorlist=https://ci:s3cret-mirror@mirrors.example.test/list?repo=private-9&token=hunter2&arch=$basearch
enabled=0

[baseos]
name=Duplicate of baseos
baseurl=https://duplicate.example.test/
`,
	"etc/yum.repos.d/README": "[ignored]\nbaseurl=https://ignored.example.test/\n",
}

func TestParseRepoFiles(t *testing.T) {
	fsys := fstest.MapFS{"usr/bin/dnf": {}}
	for name, data := range rpmRepoFiles {
		fsys[name] = &fstest.MapFile{Data: []byte(data)}
	}
	host := &Host{FS: fsys}
	repositories := host.configuredRepositories(ManagerDnf)
	got := map[string]Repository{}
	for _, repository := range repositories {
		got[repository.ID] = repository
	}
	key := func(value string) *string { return &value }
	want := map[string]Repository{
		"baseos": {ID: "baseos", File: "/etc/yum.repos.d/almalinux-baseos.repo", Type: "rpm", Enabled: true,
			URIs: []string{"mirrorlist:https://mirrors.almalinux.org/mirrorlist/$releasever/baseos"}, Name: "AlmaLinux $releasever - BaseOS",
			Signing: RepositorySigning{SignedBy: key("file:///etc/pki/rpm-gpg/RPM-GPG-KEY-AlmaLinux-9"), GPGCheck: boolPointer(true)}, Transport: "https"},
		"baseos-debuginfo": {ID: "baseos-debuginfo", File: "/etc/yum.repos.d/almalinux-baseos.repo", Type: "rpm",
			URIs: []string{"mirrorlist:https://mirrors.almalinux.org/mirrorlist/$releasever/baseos-debuginfo"}, Name: "AlmaLinux $releasever - BaseOS - Debug",
			Signing: RepositorySigning{GPGCheck: boolPointer(true)}, Transport: "https"},
		"epel": {ID: "epel", File: "/etc/yum.repos.d/epel.repo", Type: "rpm", Enabled: true,
			URIs: []string{"metalink:https://mirrors.fedoraproject.org/metalink?repo=epel-$releasever&arch=$basearch&infra=$infra&content=$contentdir"}, Name: "Extra Packages for Enterprise Linux $releasever - $basearch",
			Signing: RepositorySigning{SignedBy: key("file:///etc/pki/rpm-gpg/RPM-GPG-KEY-EPEL-$releasever"), GPGCheck: boolPointer(true)}, Transport: "https"},
		"private": {ID: "private", File: "/etc/yum.repos.d/private.repo", Type: "rpm",
			URIs: []string{"https://private.example.test/el9/$basearch/", "https://mirror.example.test/el9/$basearch/"}, Name: "Private repository",
			Signing: RepositorySigning{GPGCheck: boolPointer(false)}, Transport: "https"},
		"private-mirror": {ID: "private-mirror", File: "/etc/yum.repos.d/private.repo", Type: "rpm",
			URIs: []string{"mirrorlist:https://mirrors.example.test/list?repo=private-9&arch=$basearch"}, Name: "Private mirror list",
			Signing: RepositorySigning{GPGCheck: boolPointer(true)}, Transport: "https"},
	}
	if !reflect.DeepEqual(got, want) {
		for id := range want {
			if !reflect.DeepEqual(got[id], want[id]) {
				t.Errorf("%s =\n%+v\nwant\n%+v", id, got[id], want[id])
			}
		}
		t.Fatalf("repositories %v", slices.Sorted(func(yield func(string) bool) {
			for id := range got {
				if !yield(id) {
					return
				}
			}
		}))
	}

	// Only the allow-listed keys are kept while parsing.
	for _, section := range parseRepoINI([]byte(rpmRepoFiles["etc/yum.repos.d/private.repo"])) {
		for key, value := range section.values {
			if !rpmRepoKeys[key] || strings.Contains(value, "s3cret-password") {
				t.Errorf("kept %s=%s", key, value)
			}
		}
	}
	// Without a main config gpgcheck is unknown.
	delete(fsys, "etc/dnf/dnf.conf")
	if repository := host.configuredRepositories(ManagerDnf)[0]; repository.ID != "baseos" || *repository.Signing.GPGCheck != true {
		t.Errorf("explicit gpgcheck = %+v", repository.Signing)
	}
	for _, repository := range host.configuredRepositories(ManagerDnf) {
		if repository.ID == "baseos-debuginfo" && repository.Signing.GPGCheck != nil {
			t.Errorf("gpgcheck without dnf.conf = %v", *repository.Signing.GPGCheck)
		}
	}
}

func TestParseRepolist(t *testing.T) {
	read := func(distro string) map[string]bool {
		data, err := os.ReadFile(filepath.Join(packagesTestdata, distro, "commands", "repolist.txt"))
		if err != nil {
			t.Fatal(err)
		}
		return parseRepolist(data)
	}
	alma := read("almalinux-9")
	if len(alma) != 27 || !alma["baseos"] || !alma["appstream"] || !alma["extras"] || alma["crb"] || alma["baseos-debuginfo"] {
		t.Fatalf("alma repolist = %v", alma)
	}
	centos := read("centos-7")
	if !centos["base"] || !centos["updates"] || !centos["extras"] || centos["base-source"] || centos["c7-media"] || centos["C7.8.2003-base"] {
		t.Fatalf("centos repolist = %v", centos)
	}
	expired := parseRepolist([]byte("repo id   repo name   status\n!epel/x86_64   EPEL with enabled in its name   enabled: 13,791\n*remi   Remi   disabled\n"))
	if !reflect.DeepEqual(expired, map[string]bool{"epel": true, "remi": false}) {
		t.Fatalf("yum markers = %v", expired)
	}
}

func TestParseFromRepo(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(packagesTestdata, "almalinux-9", "commands", "repoquery-from-repo.txt"))
	if err != nil {
		t.Fatal(err)
	}
	origins := parseFromRepo(data)
	if len(origins) != 158 || !reflect.DeepEqual(origins["bash/aarch64"], []string{"baseos"}) {
		t.Fatalf("%d packages, bash %v", len(origins), origins["bash/aarch64"])
	}
	extra := parseFromRepo([]byte("kernel-core\tx86_64\tbaseos\n\nkernel-core\tx86_64\tbaseos\nkernel-core\tx86_64\t@System\nbroken line\n"))
	if !reflect.DeepEqual(extra, map[string][]string{"kernel-core/x86_64": {"baseos", "@System"}}) {
		t.Fatalf("installonly = %v", extra)
	}

	data, err = os.ReadFile(filepath.Join(packagesTestdata, "centos-7", "commands", "list-installed.txt"))
	if err != nil {
		t.Fatal(err)
	}
	yum := parseYumListInstalled(data)
	if len(yum) != 148 || !reflect.DeepEqual(yum["bash/x86_64"], []string{"CentOS"}) || !reflect.DeepEqual(yum["bind-license/noarch"], []string{"CentOS"}) {
		t.Fatalf("%d yum packages, bash %v", len(yum), yum["bash/x86_64"])
	}
	wrapped := parseYumListInstalled([]byte("Installed Packages\nperl-very-long-package-name-that-wraps.noarch\n            1.0-1.el7               @epel\nnginx.x86_64  1:1.20.1-10.el7  installed\nlocal.x86_64 1.0-1 @/local-1.0-1.x86_64\n"))
	want := map[string][]string{"perl-very-long-package-name-that-wraps/noarch": {"epel"}, "nginx/x86_64": {"installed"}, "local/x86_64": {""}}
	if !reflect.DeepEqual(wrapped, want) {
		t.Fatalf("wrapped = %v", wrapped)
	}
}

// rpmOriginsHost is an AlmaLinux 9 host with EPEL and a private repository.
func rpmOriginsHost(t *testing.T, repoquery CommandResult) (*Host, *[]string) {
	t.Helper()
	fsys := fstest.MapFS{"usr/bin/dnf": {}, "usr/bin/yum": {}, "var/lib/rpm/rpmdb.sqlite": {}}
	for name, data := range rpmRepoFiles {
		fsys[name] = &fstest.MapFile{Data: []byte(data)}
	}
	rpmQA := "bash\t(none)\t5.1.8\t9.el9\tx86_64\tbash-5.1.8-9.el9.src.rpm\n" +
		"htop\t(none)\t3.3.0\t1.el9\tx86_64\thtop-3.3.0-1.el9.src.rpm\n" +
		"tool\t(none)\t1.0\t1\tx86_64\ttool-1.0-1.src.rpm\n" +
		"legacy\t(none)\t1.0\t1\tnoarch\tlegacy-1.0-1.src.rpm\n" +
		"kickstart\t(none)\t1.0\t1\tnoarch\tkickstart-1.0-1.src.rpm\n" +
		"gone\t(none)\t1.0\t1\tnoarch\tgone-1.0-1.src.rpm\n"
	var ran []string
	host := &Host{FS: fsys, Command: func(_ context.Context, name string, args ...string) (CommandResult, error) {
		line := strings.Join(append([]string{name}, args...), " ")
		ran = append(ran, line)
		switch {
		case name == "rpm":
			return CommandResult{Stdout: []byte(rpmQA)}, nil
		case strings.Contains(line, " repoquery "):
			return repoquery, nil
		case strings.Contains(line, " repolist "):
			// The effective state: private is enabled elsewhere (a dnf
			// plugin), epel disabled.
			return CommandResult{Stdout: []byte("repo id   repo name   status\nbaseos  AlmaLinux 9 - BaseOS  enabled\nepel  EPEL  disabled\nprivate  Private  enabled\n")}, nil
		}
		return CommandResult{}, os.ErrNotExist
	}}
	return host, &ran
}

func TestRPMRepositoriesAndOrigins(t *testing.T) {
	repoquery := CommandResult{Stdout: []byte("bash\tx86_64\tbaseos\n\nhtop\tx86_64\tepel\n\ntool\tx86_64\t@commandline\n\nlegacy\tnoarch\t@System\n\nkickstart\tnoarch\tanaconda\n\ngone\tnoarch\tremoved-repo\n")}
	host, ran := rpmOriginsHost(t, repoquery)
	origins := NewOrigins(host)
	packages := collectPackagesWith(t, NewPackagesWithOrigins(host, origins))
	want := map[string][]string{"bash": {"baseos"}, "htop": {"epel"}, "tool": {}, "legacy": {}, "kickstart": {}, "gone": {}}
	for name, ids := range want {
		if got := findPackage(packages, name).Origins; !reflect.DeepEqual(got, ids) {
			t.Errorf("%s origins = %#v, want %#v", name, got, ids)
		}
	}
	payload, err := NewRepositories(host, origins).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	repositories := payload.(*RepositoriesInfo)
	if repositories.Manager != ManagerDnf || len(repositories.Items) != 5 {
		t.Fatalf("repositories = %+v", repositories)
	}
	for id, state := range map[string]struct {
		enabled  bool
		packages int
	}{"baseos": {true, 1}, "baseos-debuginfo": {false, 0}, "epel": {false, 1}, "private": {true, 0}} {
		item := findRepository(t, repositories, "", id)
		if item.Enabled != state.enabled || item.Packages != state.packages {
			t.Errorf("%s: enabled %v, %d packages", id, item.Enabled, item.Packages)
		}
	}
	wire, _ := json.Marshal(repositories)
	for _, secret := range []string{"s3cret", "hunter2", "token", "ci:", "private.key", "repo=private&"} {
		if strings.Contains(string(wire), secret) {
			t.Errorf("payload contains %q: %s", secret, wire)
		}
	}
	repoqueries := 0
	for _, line := range *ran {
		if strings.Contains(line, "repoquery") {
			repoqueries++
		}
		if strings.HasPrefix(line, "dnf ") && !strings.HasPrefix(line, "dnf -C -q --setopt=exit_on_lock=True ") {
			t.Errorf("%q may wait on a lock or the network", line)
		}
	}
	if repoqueries != 1 {
		t.Errorf("repoquery ran %d times; the index is shared", repoqueries)
	}
}

func TestRPMOriginsFailures(t *testing.T) {
	// A held lock defers both datasets; any other failure leaves the
	// origins out.
	locked := CommandResult{Stderr: []byte("metadata already locked by 34\n"), ExitCode: 200}
	host, _ := rpmOriginsHost(t, locked)
	if _, err := NewPackages(host).Collect(context.Background()); !errors.Is(err, ErrPackageManagerBusy) {
		t.Fatalf("locked packages = %v", err)
	}
	if _, err := NewRepositories(host, nil).Collect(context.Background()); !errors.Is(err, ErrPackageManagerBusy) {
		t.Fatalf("locked repositories = %v", err)
	}
	host, _ = rpmOriginsHost(t, CommandResult{ExitCode: 1, Stderr: []byte("Error: rpmdb open failed\n")})
	packages := collectPackagesWith(t, NewPackages(host))
	if wire, _ := json.Marshal(packages); strings.Contains(string(wire), "origins") {
		t.Fatalf("origins after a failure: %s", wire)
	}
}

func TestRepositoriesChangeToken(t *testing.T) {
	fsys, _, host := aptOriginsHost(t)
	collector := NewRepositories(host, nil)
	token := collector.ChangeToken()
	if collector.ChangeToken() != token {
		t.Fatal("unstable token")
	}
	fsys["etc/apt/sources.list.d/new.list"] = &fstest.MapFile{Data: []byte("deb https://new.example.test/ bookworm main\n")}
	changed := collector.ChangeToken()
	if changed == token {
		t.Fatal("a new sources file did not change the token")
	}
	fsys["var/lib/apt/lists"] = &fstest.MapFile{Mode: fs.ModeDir, ModTime: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)}
	if collector.ChangeToken() == changed {
		t.Fatal("an apt-get update did not change the token")
	}
	rpm := fstest.MapFS{"etc/yum.repos.d/epel.repo": {Data: []byte("[epel]\n")}}
	before := NewRepositories(&Host{FS: rpm}, nil).ChangeToken()
	rpm["etc/yum.repos.d/epel.repo"].ModTime = time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	if NewRepositories(&Host{FS: rpm}, nil).ChangeToken() == before {
		t.Fatal("an edited .repo file did not change the token")
	}
}
