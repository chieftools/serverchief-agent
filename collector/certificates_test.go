package collector

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"go.chief.tools/serverchief-agent/inventory"
)

// Every certificate and key in these tests is generated at run time for
// example.test names; nothing real is committed.

// pemFingerprint is the SHA-256 of the first certificate in a PEM blob.
func pemFingerprint(t *testing.T, data []byte) string {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("no PEM block")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

// testPKI is a throwaway root and intermediate CA.
type testPKI struct {
	root, intermediate       *x509.Certificate
	rootKey, intermediateKey crypto.Signer
	rootPEM, intermediatePEM []byte
}

var (
	pkiOnce   sync.Once
	sharedPKI *testPKI
)

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	pkiOnce.Do(func() {
		pki := &testPKI{}
		rootKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		rootTemplate := &x509.Certificate{
			SerialNumber:          big.NewInt(100),
			Subject:               pkix.Name{CommonName: "Example Test Root", Organization: []string{"Example Test Trust"}},
			NotBefore:             time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			NotAfter:              time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC),
			IsCA:                  true,
			BasicConstraintsValid: true,
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		}
		der, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, rootKey.Public(), rootKey)
		if err != nil {
			panic(err)
		}
		pki.root, _ = x509.ParseCertificate(der)
		pki.rootKey = rootKey
		pki.rootPEM = pemBlock("CERTIFICATE", der)

		intermediateKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		intermediateTemplate := &x509.Certificate{
			SerialNumber:          big.NewInt(200),
			Subject:               pkix.Name{CommonName: "Example Test Issuing CA", Organization: []string{"Example Test Trust"}},
			NotBefore:             time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			NotAfter:              time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC),
			IsCA:                  true,
			BasicConstraintsValid: true,
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		}
		der, err = x509.CreateCertificate(rand.Reader, intermediateTemplate, pki.root, intermediateKey.Public(), rootKey)
		if err != nil {
			panic(err)
		}
		pki.intermediate, _ = x509.ParseCertificate(der)
		pki.intermediateKey = intermediateKey
		pki.intermediatePEM = pemBlock("CERTIFICATE", der)
		sharedPKI = pki
	})
	return sharedPKI
}

// roots returns a pool that trusts only the test root.
func (pki *testPKI) roots() func() *x509.CertPool {
	return func() *x509.CertPool {
		pool := x509.NewCertPool()
		pool.AddCert(pki.root)
		return pool
	}
}

// leafOptions describe a generated leaf certificate.
type leafOptions struct {
	commonName string
	org        string
	names      []string
	ips        []net.IP
	serial     *big.Int
	notBefore  time.Time
	notAfter   time.Time
	key        string // rsa1024, rsa2048, p256 (default), p384, ed25519
	selfSigned bool
}

// leaf issues a leaf from the intermediate (or self-signed) and returns the
// certificate and key PEM.
func (pki *testPKI) leaf(t *testing.T, options leafOptions) (certificatePEM, keyPEM []byte) {
	t.Helper()
	var key crypto.Signer
	var err error
	switch options.key {
	case "rsa1024":
		key, err = rsa.GenerateKey(rand.Reader, 1024)
	case "rsa2048":
		key, err = rsa.GenerateKey(rand.Reader, 2048)
	case "p384":
		key, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	case "ed25519":
		_, key, err = ed25519.GenerateKey(rand.Reader)
	default:
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if err != nil {
		t.Fatal(err)
	}
	if options.serial == nil {
		options.serial = big.NewInt(time.Now().UnixNano())
	}
	if options.notBefore.IsZero() {
		options.notBefore = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	}
	if options.notAfter.IsZero() {
		options.notAfter = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	template := &x509.Certificate{
		SerialNumber: options.serial,
		Subject:      pkix.Name{CommonName: options.commonName},
		DNSNames:     options.names,
		IPAddresses:  options.ips,
		NotBefore:    options.notBefore,
		NotAfter:     options.notAfter,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if options.org != "" {
		template.Subject.Organization = []string{options.org}
	}
	parent, signer := pki.intermediate, pki.intermediateKey
	if options.selfSigned {
		parent, signer = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, key.Public(), signer)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pemBlock("CERTIFICATE", der), pemBlock("PRIVATE KEY", keyDER)
}

func pemDER(t *testing.T, data []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("no PEM block")
	}
	return block.Bytes
}

func concat(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

func mapFile(data []byte) *fstest.MapFile {
	return &fstest.MapFile{Data: data, Mode: 0o644, ModTime: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
}

func symlink(target string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(target), Mode: fs.ModeSymlink | 0o777}
}

// readTracker wraps a MapFS and records every buffer handed to Read, so a
// test can check that nothing read stays in memory.
type readTracker struct {
	fstest.MapFS
	mu      sync.Mutex
	buffers [][]byte
	opens   map[string]int
}

func (tracker *readTracker) Open(name string) (fs.File, error) {
	file, err := tracker.MapFS.Open(name)
	if err != nil {
		return nil, err
	}
	tracker.mu.Lock()
	if tracker.opens == nil {
		tracker.opens = map[string]int{}
	}
	tracker.opens[name]++
	tracker.mu.Unlock()
	return &trackedFile{File: file, tracker: tracker}, nil
}

type trackedFile struct {
	fs.File
	tracker *readTracker
}

func (file *trackedFile) Read(buffer []byte) (int, error) {
	file.tracker.mu.Lock()
	file.tracker.buffers = append(file.tracker.buffers, buffer)
	file.tracker.mu.Unlock()
	return file.File.Read(buffer)
}

func TestScanCertificatePEMKeepsOnlyCertificates(t *testing.T) {
	pki := newTestPKI(t)
	first, firstKey := pki.leaf(t, leafOptions{commonName: "one.example.test", names: []string{"one.example.test"}})
	second, _ := pki.leaf(t, leafOptions{commonName: "two.example.test", key: "rsa2048"})
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalECPrivateKey(ecKey)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 1024)
	rsaDER := x509.MarshalPKCS1PrivateKey(rsaKey)
	keyDERs := [][]byte{pemDER(t, firstKey), ecDER, rsaDER}

	// Every kind of block a combined file holds: keys before, between and
	// after the certificates, parameters, CRLF line ends and indentation.
	data := concat(
		firstKey,
		[]byte("subject=/CN=one.example.test\n"),
		first,
		pemBlock("EC PRIVATE KEY", ecDER),
		pemBlock("DH PARAMETERS", []byte("parameters")),
		bytes.ReplaceAll(pki.intermediatePEM, []byte("\n"), []byte("\r\n")),
		pemBlock("RSA PRIVATE KEY", rsaDER),
		bytes.ReplaceAll(second, []byte("\n"), []byte("\n  ")),
		pemBlock("TRUSTED CERTIFICATE", pemDER(t, second)),
	)
	blocks := scanCertificatePEM(data)
	want := [][]byte{pemDER(t, first), pki.intermediate.Raw, pemDER(t, second)}
	if len(blocks) != len(want) {
		t.Fatalf("got %d blocks, want %d", len(blocks), len(want))
	}
	for index := range want {
		if !bytes.Equal(blocks[index], want[index]) {
			t.Errorf("block %d differs", index)
		}
	}
	for _, block := range blocks {
		for _, key := range keyDERs {
			if bytes.Contains(block, key[8:40]) {
				t.Fatal("a returned block contains key material")
			}
		}
	}

	// Through the file reader: only certificates are kept, and the read
	// buffer, which held the keys, is wiped.
	tracker := &readTracker{MapFS: fstest.MapFS{
		"etc/ssl/combined.pem": mapFile(data),
		"etc/ssl/key-only.pem": mapFile(concat(firstKey, pemBlock("EC PRIVATE KEY", ecDER))),
	}}
	host := &Host{FS: tracker}
	file := host.readCertificateFile("/etc/ssl/combined.pem")
	if file.err != nil || len(file.certificates) != 3 || fingerprintOf(file.leaf()) != pemFingerprint(t, first) {
		t.Fatalf("combined.pem = %+v", file)
	}
	if keyOnly := host.readCertificateFile("/etc/ssl/key-only.pem"); keyOnly.err != nil || len(keyOnly.certificates) != 0 {
		t.Fatalf("key-only.pem = %+v", keyOnly)
	}
	if len(tracker.buffers) == 0 {
		t.Fatal("no reads recorded")
	}
	for _, buffer := range tracker.buffers {
		for _, value := range buffer {
			if value != 0 {
				t.Fatal("the read buffer was not cleared")
			}
		}
	}
	for _, certificate := range file.certificates {
		for _, key := range keyDERs {
			if bytes.Contains(certificate.Raw, key[8:40]) {
				t.Fatal("parsed certificates retain key bytes")
			}
		}
	}
}

func TestCertificateItemFields(t *testing.T) {
	pki := newTestPKI(t)
	cases := []struct {
		key, signature, curve string
		bits                  int
		keyType               string
	}{
		{"rsa1024", "ECDSA-SHA256", "", 1024, "rsa"},
		{"rsa2048", "ECDSA-SHA256", "", 2048, "rsa"},
		{"p256", "ECDSA-SHA256", "P-256", 256, "ecdsa"},
		{"p384", "ECDSA-SHA256", "P-384", 384, "ecdsa"},
		{"ed25519", "ECDSA-SHA256", "", 256, "ed25519"},
	}
	for _, test := range cases {
		certificatePEM, _ := pki.leaf(t, leafOptions{
			commonName: "Shop.Example.TEST",
			org:        "Example Test Ltd",
			names:      []string{"www.shop.example.test", "Shop.Example.TEST", "shop.example.test"},
			ips:        []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("2001:db8::1")},
			serial:     big.NewInt(0x04a1b2c3),
			notBefore:  time.Date(2026, 10, 23, 12, 42, 8, 0, time.FixedZone("CEST", 2*3600)),
			key:        test.key,
		})
		leaf, _ := x509.ParseCertificate(pemDER(t, certificatePEM))
		item := certificateItem(leaf)
		info := CertificatesInfo{Items: []Certificate{item}}
		info.Normalize()
		item = info.Items[0]
		if item.Fingerprint != pemFingerprint(t, certificatePEM) || item.Serial != "04a1b2c3" {
			t.Errorf("%s: fingerprint %s serial %s", test.key, item.Fingerprint, item.Serial)
		}
		if *item.Subject.CN != "Shop.Example.TEST" || *item.Subject.Org != "Example Test Ltd" || *item.Issuer.CN != "Example Test Issuing CA" || *item.Issuer.Org != "Example Test Trust" {
			t.Errorf("%s: subject %+v issuer %+v", test.key, item.Subject, item.Issuer)
		}
		if item.NotBefore != "2026-10-23T10:42:08Z" || item.NotAfter != "2027-01-01T00:00:00Z" {
			t.Errorf("%s: validity %s – %s", test.key, item.NotBefore, item.NotAfter)
		}
		if want := []string{"192.0.2.10", "2001:db8::1", "shop.example.test", "www.shop.example.test"}; !slices.Equal(item.Names, want) {
			t.Errorf("%s: names %v", test.key, item.Names)
		}
		curve := ""
		if item.Key.Curve != nil {
			curve = *item.Key.Curve
		}
		if item.Key.Type != test.keyType || item.Key.Bits != test.bits || curve != test.curve || item.SignatureAlgorithm != test.signature || item.SelfSigned {
			t.Errorf("%s: key %+v curve %q signature %s self-signed %v", test.key, item.Key, curve, item.SignatureAlgorithm, item.SelfSigned)
		}
	}

	selfSigned, _ := pki.leaf(t, leafOptions{commonName: "legacy.example.test", key: "rsa2048", serial: big.NewInt(1), selfSigned: true})
	leaf, _ := x509.ParseCertificate(pemDER(t, selfSigned))
	if item := certificateItem(leaf); !item.SelfSigned || item.Serial != "01" || item.SignatureAlgorithm != "SHA256-RSA" || item.Subject.Org != nil || len(item.Names) != 0 {
		t.Errorf("self-signed: %+v", item)
	}
}

func TestCertificateChain(t *testing.T) {
	pki := newTestPKI(t)
	leafPEM, _ := pki.leaf(t, leafOptions{commonName: "shop.example.test", names: []string{"shop.example.test"}})
	expiredPEM, _ := pki.leaf(t, leafOptions{commonName: "old.example.test", notBefore: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), notAfter: time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)})
	selfSignedPEM, _ := pki.leaf(t, leafOptions{commonName: "legacy.example.test", selfSigned: true})
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	fsys := fstest.MapFS{
		"etc/ssl/leaf-only.pem":      mapFile(leafPEM),
		"etc/ssl/fullchain.pem":      mapFile(concat(leafPEM, pki.intermediatePEM)),
		"etc/ssl/fullchain-root.pem": mapFile(concat(leafPEM, pki.intermediatePEM, pki.rootPEM)),
		"etc/ssl/chain.pem":          mapFile(pki.intermediatePEM),
		"etc/ssl/expired.pem":        mapFile(concat(expiredPEM, pki.intermediatePEM)),
		"etc/ssl/self-signed.pem":    mapFile(selfSignedPEM),
	}
	type want struct {
		complete      *bool
		intermediates int
	}
	yes, no := boolPointer(true), boolPointer(false)
	cases := map[string]struct {
		path, chain string
		roots       bool
		want        want
	}{
		"leaf only":                           {"/etc/ssl/leaf-only.pem", "", true, want{no, 0}},
		"intermediate in the same file":       {"/etc/ssl/fullchain.pem", "", true, want{yes, 1}},
		"root in the file is no intermediate": {"/etc/ssl/fullchain-root.pem", "", true, want{yes, 1}},
		"configured chain file":               {"/etc/ssl/leaf-only.pem", "/etc/ssl/chain.pem", true, want{yes, 1}},
		"expired but complete":                {"/etc/ssl/expired.pem", "", true, want{yes, 1}},
		"self-signed":                         {"/etc/ssl/self-signed.pem", "", true, want{nil, 0}},
		"no trust anchors":                    {"/etc/ssl/fullchain.pem", "", false, want{nil, 1}},
	}
	for name, test := range cases {
		host := &Host{FS: fsys, Now: func() time.Time { return now }}
		if test.roots {
			host.Roots = pki.roots()
		}
		run := newCertificateRun(host, nil)
		run.add(certificateSource{path: test.path, chain: test.chain}, CertificateUse{Kind: CertificateUseOther, Ref: "test"})
		if len(run.entries) != 1 {
			t.Fatalf("%s: %d entries", name, len(run.entries))
		}
		for _, entry := range run.entries {
			got := entry.chain
			if (got.Complete == nil) != (test.want.complete == nil) || got.Complete != nil && *got.Complete != *test.want.complete || got.Intermediates != test.want.intermediates {
				t.Errorf("%s: chain %+v (complete %v), want %+v", name, got, deref(got.Complete), test.want)
			}
		}
	}

	// A certificate configured in two places is complete when one of them
	// is.
	host := &Host{FS: fsys, Now: func() time.Time { return now }, Roots: pki.roots()}
	run := newCertificateRun(host, nil)
	run.add(certificateSource{path: "/etc/ssl/leaf-only.pem"}, CertificateUse{Kind: CertificateUseSite, Ref: "nginx/shop.example.test"})
	run.add(certificateSource{path: "/etc/ssl/fullchain.pem"}, CertificateUse{Kind: CertificateUsePostfix, Ref: "postfix"})
	run.add(certificateSource{path: "/etc/postfix/../ssl/fullchain.pem"}, CertificateUse{Kind: CertificateUseDovecot, Ref: "dovecot"})
	entry := run.entries[pemFingerprint(t, leafPEM)]
	if len(run.entries) != 1 || entry.chain.Complete == nil || !*entry.chain.Complete || entry.chain.Intermediates != 1 || len(sortedUnique(entry.paths)) != 2 || len(entry.uses) != 3 {
		t.Fatalf("merged entry = %+v", entry)
	}
}

func deref(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}

func TestCertificateFilePolicy(t *testing.T) {
	pki := newTestPKI(t)
	leafPEM, _ := pki.leaf(t, leafOptions{commonName: "alice.example.test"})
	large := concat(leafPEM, bytes.Repeat([]byte("# padding\n"), maxCertificateFile/10+1))
	fsys := fstest.MapFS{
		"etc/letsencrypt/archive/alice.example.test/cert1.pem": mapFile(leafPEM),
		"etc/letsencrypt/live/alice.example.test/cert.pem":     symlink("../../archive/alice.example.test/cert1.pem"),
		"etc/ssl/large.pem":                       mapFile(large),
		"etc/ssl/other.pem":                       mapFile(leafPEM),
		"etc/ssl/to-home.pem":                     symlink("/home/alice/ssl/escape.pem"),
		"home/alice/ssl/cert.pem":                 mapFile(leafPEM),
		"home/alice/ssl/inside.pem":               symlink("cert.pem"),
		"home/alice/ssl/nested.pem":               symlink("/home/alice/ssl/inside.pem"),
		"home/alice/ssl/escape.pem":               symlink("/etc/ssl/other.pem"),
		"home/alice/ssl/dotdot.pem":               symlink("../../../etc/ssl/other.pem"),
		"home/alice/ssl/self":                     symlink("/home/alice"),
		"home/alice/ssl/via-self.pem":             symlink("self/../bob/cert.pem"),
		"home/bob/cert.pem":                       mapFile(leafPEM),
		"home/alice/ssl/loop-a.pem":               symlink("loop-b.pem"),
		"home/alice/ssl/loop-b.pem":               symlink("loop-a.pem"),
		"var/www/vhosts/shop.example.test/x.pem":  symlink("/var/www/vhosts/other.example.test/y.pem"),
		"var/www/vhosts/other.example.test/y.pem": mapFile(leafPEM),
		"etc/ssl/fifo.pem":                        &fstest.MapFile{Mode: fs.ModeNamedPipe},
	}
	host := &Host{FS: fsys}
	fingerprint := pemFingerprint(t, leafPEM)
	for _, name := range []string{
		"/etc/letsencrypt/live/alice.example.test/cert.pem", // root-owned links are followed
		"/home/alice/ssl/cert.pem",
		"/home/alice/ssl/inside.pem", // a link that stays in the user's tree
		"/home/alice/ssl/nested.pem",
	} {
		if file := host.readCertificateFile(name); file.err != nil || fingerprintOf(file.leaf()) != fingerprint {
			t.Errorf("%s: %v", name, file.err)
		}
	}
	for name, message := range map[string]string{
		"/etc/ssl/large.pem":                      "larger than 1 MiB",
		"/home/alice/ssl/escape.pem":              "not following symbolic link /home/alice/ssl/escape.pem -> /etc/ssl/other.pem out of /home/alice",
		"/etc/ssl/to-home.pem":                    "out of /home/alice",
		"/home/alice/ssl/dotdot.pem":              "out of /home/alice",
		"/home/alice/ssl/via-self.pem":            "out of /home/alice",
		"/home/alice/ssl/loop-a.pem":              "too many levels of symbolic links",
		"/var/www/vhosts/shop.example.test/x.pem": "out of /var/www/vhosts/shop.example.test",
		"/etc/ssl/fifo.pem":                       "not a regular file",
	} {
		file := host.readCertificateFile(name)
		if file.err == nil || !strings.Contains(certificateWarning(name, file.err), message) || len(file.certificates) != 0 {
			t.Errorf("%s: err %v, want %q", name, file.err, message)
		}
	}
	if warning := certificateWarning("/missing.pem", host.readCertificateFile("/etc/ssl/missing.pem").err); warning != "" {
		t.Errorf("missing file warns: %s", warning)
	}
}

func TestCertificateStoreReusesParsedFiles(t *testing.T) {
	pki := newTestPKI(t)
	first, _ := pki.leaf(t, leafOptions{commonName: "one.example.test"})
	second, _ := pki.leaf(t, leafOptions{commonName: "two.example.test", key: "p384"})
	tracker := &readTracker{MapFS: fstest.MapFS{"etc/ssl/site.pem": mapFile(first)}}
	host := &Host{FS: tracker}
	store := newCertificateStore()
	for range 3 {
		if leaf := store.load(host, "/etc/ssl/site.pem").leaf(); leaf == nil || fingerprintOf(leaf) != pemFingerprint(t, first) {
			t.Fatal("first load")
		}
	}
	if tracker.opens["etc/ssl/site.pem"] != 1 {
		t.Fatalf("read %d times, want once", tracker.opens["etc/ssl/site.pem"])
	}
	tracker.MapFS["etc/ssl/site.pem"] = &fstest.MapFile{Data: second, ModTime: time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)}
	if leaf := store.load(host, "/etc/ssl/site.pem").leaf(); leaf == nil || fingerprintOf(leaf) != pemFingerprint(t, second) {
		t.Fatal("a changed file was not read again")
	}
}

func TestCAFromURLAndIssuer(t *testing.T) {
	for directory, want := range map[string]string{
		"https://acme-v02.api.letsencrypt.org/directory":         "letsencrypt",
		"https://acme-staging-v02.api.letsencrypt.org/directory": "letsencrypt-staging",
		"https://acme.zerossl.com/v2/DV90":                       "zerossl",
		"https://api.buypass.com/acme/directory":                 "buypass",
		"https://dv.acme-v02.api.pki.goog/directory":             "google",
		"https://acme.sectigo.com/v2/DV":                         "sectigo",
		"https://acme.example.test/directory":                    "",
	} {
		if got := caFromURL(directory); got != want {
			t.Errorf("caFromURL(%s) = %q, want %q", directory, got, want)
		}
	}
	for _, test := range []struct{ org, cn, want string }{
		{"Let's Encrypt", "R11", "letsencrypt"},
		{"(STAGING) Let's Encrypt", "(STAGING) Ersatz Edamame E1", "letsencrypt-staging"},
		{"ZeroSSL", "ZeroSSL RSA Domain Secure Site CA", "zerossl"},
		{"Buypass AS-983163327", "Buypass Class 2 CA 5", "buypass"},
		{"Google Trust Services", "WR1", "google"},
		{"Sectigo Limited", "Sectigo RSA Domain Validation Secure Server CA", "sectigo"},
		{"Example Test Trust", "Example Test Issuing CA", ""},
	} {
		if got := caFromIssuer(test.org, test.cn); got != test.want {
			t.Errorf("caFromIssuer(%q) = %q, want %q", test.org, got, test.want)
		}
	}
}

func certificatesByFingerprint(t *testing.T, info *CertificatesInfo) map[string]Certificate {
	t.Helper()
	result := map[string]Certificate{}
	for _, item := range info.Items {
		result[item.Fingerprint] = item
	}
	return result
}

func collectCertificates(t *testing.T, collector *Certificates) *CertificatesInfo {
	t.Helper()
	payload, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info := payload.(*CertificatesInfo)
	if !slices.IsSortedFunc(info.Items, func(a, b Certificate) int { return strings.Compare(a.Fingerprint, b.Fingerprint) }) {
		t.Fatal("items are not sorted by fingerprint")
	}
	return info
}

func stringValue(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
}

func managerString(manager *CertificateManager) string {
	if manager == nil {
		return "<nil>"
	}
	return strings.Join([]string{
		stringValue(manager.Tool), stringValue(manager.Name), stringValue(manager.Plugin), stringValue(manager.Installer),
		stringValue(manager.CA), stringValue(manager.CAURL), stringValue(manager.KeyType), stringValue(manager.Renewal), stringValue(manager.Config),
	}, "|")
}

func usesString(uses []CertificateUse) string {
	parts := make([]string, len(uses))
	for index, use := range uses {
		parts[index] = use.Kind + ":" + use.Ref
	}
	return strings.Join(parts, ",")
}

// nginxCertbotHost has one nginx site and Postfix sharing a certbot
// certificate, a self-signed site, and certbot's renewal configuration.
func nginxCertbotHost(t *testing.T, pki *testPKI, extra fstest.MapFS) (*Host, *readTracker, []byte, []byte) {
	t.Helper()
	shop, shopKey := pki.leaf(t, leafOptions{commonName: "shop.example.test", names: []string{"shop.example.test", "www.shop.example.test"}})
	legacy, legacyKey := pki.leaf(t, leafOptions{commonName: "legacy.example.test", names: []string{"legacy.example.test"}, key: "rsa1024", selfSigned: true})
	fsys := fstest.MapFS{
		"usr/sbin/nginx":       mapFile(nil),
		"etc/nginx/nginx.conf": mapFile([]byte("events {}\nhttp {\n\tinclude /etc/nginx/sites-enabled/*;\n}\n")),
		"etc/nginx/sites-enabled/shop": mapFile([]byte(`server {
	listen 443 ssl;
	server_name shop.example.test www.shop.example.test;
	ssl_certificate /etc/letsencrypt/live/shop.example.test/fullchain.pem;
	ssl_certificate_key /etc/letsencrypt/live/shop.example.test/privkey.pem;
}
`)),
		"etc/nginx/sites-enabled/legacy": mapFile([]byte(`server {
	listen 443 ssl;
	server_name legacy.example.test;
	ssl_certificate /etc/ssl/legacy.example.test.pem;
}
`)),
		"etc/ssl/legacy.example.test.pem":                          mapFile(concat(legacyKey, legacy)),
		"etc/letsencrypt/archive/shop.example.test/cert1.pem":      mapFile(shop),
		"etc/letsencrypt/archive/shop.example.test/fullchain1.pem": mapFile(concat(shop, pki.intermediatePEM)),
		"etc/letsencrypt/archive/shop.example.test/privkey1.pem":   mapFile(shopKey),
		"etc/letsencrypt/live/shop.example.test/cert.pem":          symlink("../../archive/shop.example.test/cert1.pem"),
		"etc/letsencrypt/live/shop.example.test/fullchain.pem":     symlink("../../archive/shop.example.test/fullchain1.pem"),
		"etc/letsencrypt/live/shop.example.test/privkey.pem":       symlink("../../archive/shop.example.test/privkey1.pem"),
		"etc/letsencrypt/renewal/shop.example.test.conf": mapFile([]byte(`# renew_before_expiry = 30 days
version = 2.1.0
archive_dir = /etc/letsencrypt/archive/shop.example.test
cert = /etc/letsencrypt/live/shop.example.test/cert.pem
privkey = /etc/letsencrypt/live/shop.example.test/privkey.pem
chain = /etc/letsencrypt/live/shop.example.test/chain.pem
fullchain = /etc/letsencrypt/live/shop.example.test/fullchain.pem

# Options used in the renewal process
[renewalparams]
account = 0123456789abcdef0123456789abcdef
authenticator = dns-domainchief
installer = nginx
server = https://acme-staging-v02.api.letsencrypt.org/directory
key_type = ecdsa
`)),
		"etc/postfix/main.cf": mapFile([]byte(`smtpd_tls_cert_file = /etc/letsencrypt/live/shop.example.test/fullchain.pem
smtpd_tls_key_file = /etc/letsencrypt/live/shop.example.test/privkey.pem
`)),
		"lib/systemd/system/certbot.timer":                     mapFile([]byte("[Timer]\n")),
		"etc/systemd/system/timers.target.wants/certbot.timer": symlink("/lib/systemd/system/certbot.timer"),
		"etc/cron.d/certbot":                                   mapFile([]byte("0 */12 * * * root test -x /usr/bin/certbot -a \\! -d /run/systemd/system && certbot -q renew\n")),
	}
	for name, file := range extra {
		fsys[name] = file
	}
	tracker := &readTracker{MapFS: fsys}
	var ran []string
	host := &Host{
		FS:      tracker,
		Command: vhostCommands(t, "", []vhostCommand{{prefix: "nginx -V", data: []byte("configure arguments: --conf-path=/etc/nginx/nginx.conf\n")}}, &ran),
		Now:     func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) },
		Roots:   pki.roots(),
	}
	return host, tracker, shop, legacy
}

func TestCertificatesNginxCertbotAndPostfix(t *testing.T) {
	pki := newTestPKI(t)
	host, tracker, shop, legacy := nginxCertbotHost(t, pki, nil)
	vhosts := NewVHosts(host)
	sites := collectVHosts(t, vhosts)
	collector := NewCertificates(host, vhosts)
	info := collectCertificates(t, collector)
	items := certificatesByFingerprint(t, info)
	if len(items) != 2 {
		t.Fatalf("items = %+v", info.Items)
	}

	shopItem := items[pemFingerprint(t, shop)]
	if got := usesString(shopItem.UsedBy); got != "postfix:postfix,site:nginx/shop.example.test" {
		t.Errorf("shop used_by = %s", got)
	}
	if !slices.Equal(shopItem.Paths, []string{"/etc/letsencrypt/live/shop.example.test/fullchain.pem"}) {
		t.Errorf("shop paths = %v", shopItem.Paths)
	}
	if shopItem.Chain.Complete == nil || !*shopItem.Chain.Complete || shopItem.Chain.Intermediates != 1 {
		t.Errorf("shop chain = %+v", shopItem.Chain)
	}
	if got, want := managerString(shopItem.ManagedBy), "certbot|shop.example.test|dns-domainchief|nginx|letsencrypt-staging|https://acme-staging-v02.api.letsencrypt.org/directory|ecdsa|timer|/etc/letsencrypt/renewal/shop.example.test.conf"; got != want {
		t.Errorf("shop managed_by\n got %s\nwant %s", got, want)
	}

	legacyItem := items[pemFingerprint(t, legacy)]
	if !legacyItem.SelfSigned || legacyItem.Chain.Complete != nil || legacyItem.ManagedBy != nil || legacyItem.Key.Bits != 1024 || usesString(legacyItem.UsedBy) != "site:nginx/legacy.example.test" {
		t.Errorf("legacy = %+v", legacyItem)
	}

	// The vhosts dataset links the sites by fingerprint.
	for _, site := range sites.Items {
		if site.Certificate == nil || items[site.Certificate.Fingerprint].Fingerprint == "" {
			t.Errorf("site %s has no matching certificate", VHostKey(site))
		}
	}
	// vhosts and certificates share one parse of each file; private keys
	// are never opened.
	for name, count := range tracker.opens {
		if strings.Contains(name, "privkey") || strings.HasSuffix(name, ".key") {
			t.Errorf("opened %s", name)
		}
		if strings.HasSuffix(name, ".pem") && count > 1 {
			t.Errorf("%s read %d times", name, count)
		}
	}

	// Collecting again changes nothing.
	again := collectCertificates(t, collector)
	if certificatesHash(t, info) != certificatesHash(t, again) {
		t.Fatal("hash changed between identical collections")
	}
}

func certificatesHash(t *testing.T, info *CertificatesInfo) string {
	t.Helper()
	dataset, err := inventory.NewDataset("certificates", info)
	if err != nil {
		t.Fatal(err)
	}
	return dataset.Hash
}

func TestCertbotRenewalDetection(t *testing.T) {
	pki := newTestPKI(t)
	cases := map[string]struct {
		change fstest.MapFS
		remove []string
		want   string
	}{
		"enabled timer": {want: CertificateRenewalTimer},
		"masked timer, guarded cron line on systemd": {
			change: fstest.MapFS{"etc/systemd/system/certbot.timer": symlink("/dev/null"), "run/systemd/system": &fstest.MapFile{Mode: fs.ModeDir | 0o755}},
			want:   CertificateRenewalNone,
		},
		"guarded cron line without systemd": {
			remove: []string{"etc/systemd/system/timers.target.wants/certbot.timer"},
			want:   CertificateRenewalCron,
		},
		"snap timer": {
			remove: []string{"etc/systemd/system/timers.target.wants/certbot.timer", "etc/cron.d/certbot"},
			change: fstest.MapFS{"etc/systemd/system/timers.target.wants/snap.certbot.renew.timer": symlink("/etc/systemd/system/snap.certbot.renew.timer")},
			want:   CertificateRenewalTimer,
		},
		"root crontab": {
			remove: []string{"etc/systemd/system/timers.target.wants/certbot.timer", "etc/cron.d/certbot"},
			change: fstest.MapFS{"var/spool/cron/crontabs/root": mapFile([]byte("# m h dom mon dow command\n17 3 * * * /usr/bin/certbot renew --quiet --post-hook 'systemctl reload nginx'\n"))},
			want:   CertificateRenewalCron,
		},
		"certbot-auto script run monthly by run-parts": {
			remove: []string{"etc/systemd/system/timers.target.wants/certbot.timer", "etc/cron.d/certbot"},
			change: fstest.MapFS{"etc/cron.monthly/renew-ssl-certificates": &fstest.MapFile{Mode: 0o755, Data: []byte("#!/bin/bash\n\n/opt/certbot/letsencrypt-auto certonly -c /etc/certbot/shop.example.test.conf --renew-by-default\n\nservice nginx reload\n")}},
			want:   CertificateRenewalCron,
		},
		"script run-parts skips as not executable": {
			remove: []string{"etc/systemd/system/timers.target.wants/certbot.timer", "etc/cron.d/certbot"},
			change: fstest.MapFS{"etc/cron.daily/certbot": &fstest.MapFile{Mode: 0o644, Data: []byte("#!/bin/sh\ncertbot renew\n")}},
			want:   CertificateRenewalNone,
		},
		"commented cron line": {
			remove: []string{"etc/systemd/system/timers.target.wants/certbot.timer", "etc/cron.d/certbot"},
			change: fstest.MapFS{"etc/crontab": mapFile([]byte("# 17 3 * * * root certbot renew\n"))},
			want:   CertificateRenewalNone,
		},
	}
	for name, test := range cases {
		host, tracker, shop, _ := nginxCertbotHost(t, pki, test.change)
		for _, removed := range test.remove {
			delete(tracker.MapFS, removed)
		}
		items := certificatesByFingerprint(t, collectCertificates(t, NewCertificates(host, nil)))
		if got := stringValue(items[pemFingerprint(t, shop)].ManagedBy.Renewal); got != test.want {
			t.Errorf("%s: renewal %s, want %s", name, got, test.want)
		}
	}
}

func TestCertificatesAcmeSh(t *testing.T) {
	pki := newTestPKI(t)
	mail, mailKey := pki.leaf(t, leafOptions{commonName: "mail.example.test", names: []string{"mail.example.test"}, key: "rsa2048"})
	wildcard, _ := pki.leaf(t, leafOptions{commonName: "*.example.test", names: []string{"*.example.test"}, key: "p384"})
	reload := "__ACME_BASE64__START_" + "c3lzdGVtY3RsIHJlbG9hZCBkb3ZlY290" + "__ACME_BASE64__END_"
	fsys := fstest.MapFS{
		"etc/passwd":                 mapFile([]byte("root:x:0:0:root:/root:/bin/bash\n")),
		"root/.acme.sh/account.conf": mapFile([]byte("ACCOUNT_EMAIL='admin@example.test'\nDEFAULT_ACME_SERVER='https://acme-v02.api.letsencrypt.org/directory'\n")),
		"root/.acme.sh/ca/acme.zerossl.com/v2/DV90/account.key": mapFile([]byte("never read")),
		"root/.acme.sh/mail.example.test/mail.example.test.conf": mapFile([]byte(`Le_Domain='mail.example.test'
Le_Alt='no'
Le_Webroot='dns_cf'
Le_PreHook=''
Le_ReloadCmd='` + reload + `'
Le_API='https://acme.zerossl.com/v2/DV90'
Le_Keylength='2048'
Le_RealFullChainPath='/etc/ssl/mail/fullchain.pem'
`)),
		"root/.acme.sh/mail.example.test/mail.example.test.cer": mapFile(mail),
		"root/.acme.sh/mail.example.test/mail.example.test.key": mapFile(mailKey),
		"root/.acme.sh/*.example.test_ecc/*.example.test.conf":  mapFile([]byte("Le_Domain='*.example.test'\nLe_Webroot='/var/www/html'\nLe_Keylength='ec-384'\n")),
		"root/.acme.sh/*.example.test_ecc/*.example.test.cer":   mapFile(wildcard),
		"etc/ssl/mail/fullchain.pem":                            mapFile(concat(mail, pki.intermediatePEM)),
		"etc/ssl/wildcard/fullchain.pem":                        mapFile(concat(wildcard, pki.intermediatePEM)),
		"etc/dovecot/dovecot.conf":                              mapFile([]byte("!include conf.d/*.conf\n!include_try local.conf\n")),
		"etc/dovecot/conf.d/10-ssl.conf":                        mapFile([]byte("ssl = required\nssl_cert = </etc/ssl/mail/fullchain.pem\nssl_key = </etc/ssl/mail/privkey.pem\nlocal_name wild.example.test {\n  ssl_cert = </etc/ssl/wildcard/fullchain.pem\n}\n")),
		"var/spool/cron/root":                                   mapFile([]byte("12 0 * * * \"/root/.acme.sh\"/acme.sh --cron --home \"/root/.acme.sh\" > /dev/null\n")),
	}
	tracker := &readTracker{MapFS: fsys}
	host := &Host{FS: tracker, Roots: pki.roots(), Now: func() time.Time { return time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC) }}
	items := certificatesByFingerprint(t, collectCertificates(t, NewCertificates(host, nil)))
	if len(items) != 2 {
		t.Fatalf("items = %v", items)
	}
	if got, want := managerString(items[pemFingerprint(t, mail)].ManagedBy), "acme.sh|mail.example.test|dns_cf|<nil>|zerossl|https://acme.zerossl.com/v2/DV90|2048|cron|/root/.acme.sh/mail.example.test/mail.example.test.conf"; got != want {
		t.Errorf("mail managed_by\n got %s\nwant %s", got, want)
	}
	if got, want := managerString(items[pemFingerprint(t, wildcard)].ManagedBy), "acme.sh|*.example.test|webroot|<nil>|letsencrypt|https://acme-v02.api.letsencrypt.org/directory|ec-384|cron|/root/.acme.sh/*.example.test_ecc/*.example.test.conf"; got != want {
		t.Errorf("wildcard managed_by\n got %s\nwant %s", got, want)
	}
	if got := usesString(items[pemFingerprint(t, mail)].UsedBy); got != "dovecot:dovecot" {
		t.Errorf("mail used_by = %s", got)
	}
	for name := range tracker.opens {
		if strings.HasSuffix(name, ".key") {
			t.Errorf("opened %s", name)
		}
	}

	// Without the crontab line, renewal is none.
	delete(tracker.MapFS, "var/spool/cron/root")
	items = certificatesByFingerprint(t, collectCertificates(t, NewCertificates(host, nil)))
	if got := stringValue(items[pemFingerprint(t, mail)].ManagedBy.Renewal); got != CertificateRenewalNone {
		t.Errorf("renewal without cron = %s", got)
	}
}

func TestPostfixCertificates(t *testing.T) {
	cases := map[string]struct {
		mainCF   string
		postconf []vhostCommand
		want     []string
	}{
		"main.cf with references and continuations": {
			mainCF: "# TLS\ncert_dir = ${config_directory}/tls\nsmtpd_tls_cert_file =\n    $cert_dir/rsa.pem\nsmtpd_tls_eccert_file = $(cert_dir)/ecdsa.pem\n",
			want:   []string{"/etc/postfix/tls/rsa.pem", "/etc/postfix/tls/ecdsa.pem"},
		},
		"chain files replace the cert files": {
			mainCF: "smtpd_tls_cert_file = /etc/ssl/old.pem\nsmtpd_tls_chain_files = /etc/postfix/rsa-chain.pem, /etc/postfix/ec-chain.pem\n",
			want:   []string{"/etc/postfix/rsa-chain.pem", "/etc/postfix/ec-chain.pem"},
		},
		"unresolvable reference": {
			mainCF: "smtpd_tls_cert_file = $unknown/cert.pem\n",
		},
		"postconf -x": {
			postconf: []vhostCommand{{prefix: "postconf -x", data: []byte("smtpd_tls_cert_file = /etc/ssl/mail.example.test.pem\nsmtpd_tls_eccert_file =\nsmtpd_tls_chain_files =\n")}},
			want:     []string{"/etc/ssl/mail.example.test.pem"},
		},
		"postconf without -x": {
			postconf: []vhostCommand{
				{prefix: "postconf -x", data: []byte("postconf: invalid option -- 'x'\n"), exit: 1},
				{prefix: "postconf smtpd_tls_cert_file", data: []byte("smtpd_tls_cert_file = /etc/ssl/mail.example.test.pem\n")},
			},
			want: []string{"/etc/ssl/mail.example.test.pem"},
		},
	}
	for name, test := range cases {
		fsys := fstest.MapFS{"etc/postfix/main.cf": mapFile([]byte(test.mainCF))}
		host := &Host{FS: fsys}
		if test.postconf != nil {
			fsys["usr/sbin/postconf"] = mapFile(nil)
			host.Command = vhostCommands(t, "", test.postconf, new([]string))
		}
		var got []string
		for _, service := range postfixCertificates(context.Background(), newCertificateRun(host, nil)) {
			got = append(got, service.path)
		}
		if !slices.Equal(got, test.want) {
			t.Errorf("%s: %v, want %v", name, got, test.want)
		}
	}
}

func TestDovecotCertificates(t *testing.T) {
	fsys := fstest.MapFS{
		"etc/dovecot/dovecot.conf":       mapFile([]byte("protocols = imap\n!include conf.d/*.conf\n!include_try /etc/dovecot/missing.conf\n# ssl_cert = </etc/ssl/commented.pem\n")),
		"etc/dovecot/conf.d/10-ssl.conf": mapFile([]byte("ssl_cert = </etc/ssl/mail.pem\nssl_alt_cert = \"</etc/ssl/mail-ec.pem\"\n!include ../extra/*.conf\n")),
		"etc/dovecot/conf.d/20-new.conf": mapFile([]byte("ssl_server_cert_file = /etc/ssl/dovecot24.pem\nssl_cert = </etc/ssl/%{local_name}.pem\n")),
		"etc/dovecot/extra/loop.conf":    mapFile([]byte("ssl_cert = </etc/ssl/extra.pem\n!include loop.conf\n")),
	}
	run := newCertificateRun(&Host{FS: fsys}, nil)
	var got []string
	for _, service := range dovecotCertificates(run) {
		got = append(got, service.path)
	}
	want := []string{"/etc/ssl/mail.pem", "/etc/ssl/mail-ec.pem", "/etc/ssl/extra.pem", "/etc/ssl/extra.pem", "/etc/ssl/extra.pem", "/etc/ssl/dovecot24.pem"}
	if !slices.Equal(got, want) {
		t.Errorf("paths %v, want %v", got, want)
	}
	if !slices.ContainsFunc(run.warnings, func(warning string) bool { return strings.Contains(warning, "include levels") }) {
		t.Errorf("no warning for the include loop: %v", run.warnings)
	}
}

func TestDovecotFollowsPerDomainSNIFiles(t *testing.T) {
	// Plesk writes one Dovecot SNI file per mail domain, often hundreds.
	fsys := fstest.MapFS{"etc/dovecot/dovecot.conf": mapFile([]byte("!include conf.d/*.conf\n"))}
	for index := range 300 {
		name := fmt.Sprintf("mail%03d.example.test", index)
		fsys["etc/dovecot/conf.d/sni-"+name+".conf"] = mapFile([]byte("local_name " + name + " {\n  ssl_cert = </etc/ssl/" + name + ".pem\n}\n"))
	}
	run := newCertificateRun(&Host{FS: fsys}, nil)
	if got := len(dovecotCertificates(run)); got != 300 {
		t.Errorf("found %d certificates, want 300 (warnings: %v)", got, run.warnings)
	}
	if len(run.warnings) != 0 {
		t.Errorf("unexpected warnings: %v", run.warnings)
	}
}

func TestEximCertificates(t *testing.T) {
	fsys := fstest.MapFS{
		"var/lib/exim4/config.autogenerated": mapFile([]byte(`CONFDIR = /etc/exim4
MAIN_TLS_ENABLE = yes
.ifdef MAIN_TLS_ENABLE
.ifdef MAIN_TLS_CERTIFICATE
tls_certificate = MAIN_TLS_CERTIFICATE
.else
tls_certificate = CONFDIR/exim.crt
.endif
.endif
.ifdef UNDEFINED_MACRO
tls_certificate = /etc/ssl/never.pem
.endif
tls_certificate = ${if exists{/etc/ssl/sni/${tls_in_sni}.pem}{/etc/ssl/sni/${tls_in_sni}.pem}{/etc/ssl/default.pem}}
.include /etc/exim4/conf.d/main/tls
begin acl
tls_certificate = /etc/ssl/not-main.pem
`)),
		"etc/exim4/conf.d/main/tls": mapFile([]byte("tls_certificate = /etc/ssl/rsa.pem : \\\n  /etc/ssl/ecdsa.pem\n")),
	}
	var got []string
	for _, service := range eximCertificates(newCertificateRun(&Host{FS: fsys}, nil)) {
		got = append(got, service.path)
	}
	if want := []string{"/etc/exim4/exim.crt", "/etc/ssl/rsa.pem", "/etc/ssl/ecdsa.pem"}; !slices.Equal(got, want) {
		t.Errorf("paths %v, want %v", got, want)
	}
}

func TestCertificatesPlesk(t *testing.T) {
	pki := newTestPKI(t)
	site, _ := pki.leaf(t, leafOptions{commonName: "plesk-site.example.test", names: []string{"plesk-site.example.test"}})
	panel, panelKey := pki.leaf(t, leafOptions{commonName: "panel.example.test"})
	domains := mustRead(t, "testdata/vhosts/plesk/commands/domains.txt")
	domains = bytes.ReplaceAll(domains, []byte("@CERT_HEX@"), []byte(strings.ToUpper(hex.EncodeToString(site))))
	// The ca_cert column of the Let's Encrypt certificate holds the chain.
	domains = bytes.Replace(domains, []byte("Lets Encrypt example.com\tNULL"), []byte("Lets Encrypt example.com\t"+hex.EncodeToString(pki.intermediatePEM)), 1)
	var ran []string
	host := pleskHost(t, []vhostCommand{
		{prefix: "plesk db -Ne SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.COLUMNS", file: "columns.txt"},
		{prefix: "plesk db -Ne SELECT d.id, d.name", data: domains},
		{prefix: "plesk db -Ne SELECT dom_id, name FROM domain_aliases", file: "aliases.txt"},
	}, &ran)
	// The panel's httpsd.pem holds the key before the certificate.
	host.FS.(fstest.MapFS)["usr/local/psa/admin/conf/httpsd.pem"] = mapFile(concat(panelKey, panel))
	host.Roots = pki.roots()
	items := certificatesByFingerprint(t, collectCertificates(t, NewCertificates(host, nil)))
	siteItem := items[pemFingerprint(t, site)]
	if got, want := managerString(siteItem.ManagedBy), "plesk|Lets Encrypt example.com|<nil>|<nil>|<nil>|<nil>|<nil>|panel|<nil>"; got != want {
		t.Errorf("site managed_by\n got %s\nwant %s", got, want)
	}
	if usesString(siteItem.UsedBy) != "site:plesk/example.com" || len(siteItem.Paths) != 0 || siteItem.Chain.Complete == nil || !*siteItem.Chain.Complete {
		t.Errorf("site = %+v", siteItem)
	}
	panelItem := items[pemFingerprint(t, panel)]
	if usesString(panelItem.UsedBy) != "panel:plesk" || panelItem.ManagedBy != nil || !slices.Equal(panelItem.Paths, []string{pleskPanelCertificate}) {
		t.Errorf("panel = %+v", panelItem)
	}
	for _, command := range ran {
		if upper := strings.ToUpper(command); strings.Contains(upper, "PVT_KEY") || strings.Contains(upper, "CERT_FILE") {
			t.Errorf("query selects a private column: %s", command)
		}
	}
}

func TestCertificatesDirectAdmin(t *testing.T) {
	pki := newTestPKI(t)
	alice, aliceKey := pki.leaf(t, leafOptions{commonName: "alice.example.test", names: []string{"alice.example.test", "www.alice.example.test"}})
	panel, panelKey := pki.leaf(t, leafOptions{commonName: "server.example.test"})
	recorder := &openRecorder{FS: vhostFixture(t, "directadmin", map[string][]byte{
		"usr/local/directadmin/data/users/alice/domains/alice.example.cert":               alice,
		"usr/local/directadmin/data/users/alice/domains/alice.example.cacert":             pki.intermediatePEM,
		"usr/local/directadmin/data/users/alice/domains/alice.example.key":                aliceKey,
		"usr/local/directadmin/data/users/alice/domains/alice.example.combined":           concat(aliceKey, alice),
		"usr/local/directadmin/data/users/alice/domains/alice.example.cert.creation_time": []byte("1792800000\n"),
		"usr/local/directadmin/conf/directadmin.conf":                                     []byte("cacert=/usr/local/directadmin/conf/cacert.pem\ncakey=/usr/local/directadmin/conf/cakey.pem\n"),
		"usr/local/directadmin/conf/cacert.pem":                                           concat(panelKey, panel),
		"usr/local/directadmin/conf/cakey.pem":                                            panelKey,
	})}
	host := &Host{FS: recorder, Roots: pki.roots()}
	items := certificatesByFingerprint(t, collectCertificates(t, NewCertificates(host, nil)))
	aliceItem := items[pemFingerprint(t, alice)]
	if got, want := managerString(aliceItem.ManagedBy), "directadmin|alice.example|<nil>|<nil>|<nil>|<nil>|<nil>|panel|/usr/local/directadmin/data/users/alice/domains/alice.example.cert.creation_time"; got != want {
		t.Errorf("alice managed_by\n got %s\nwant %s", got, want)
	}
	if got := usesString(aliceItem.UsedBy); got != "site:directadmin/alice.example,site:directadmin/blog.alice.example,site:directadmin/staging.alice.example" {
		t.Errorf("alice used_by = %s", got)
	}
	if aliceItem.Chain.Complete == nil || !*aliceItem.Chain.Complete || aliceItem.Chain.Intermediates != 1 {
		t.Errorf("alice chain = %+v", aliceItem.Chain)
	}
	if panelItem := items[pemFingerprint(t, panel)]; usesString(panelItem.UsedBy) != "panel:directadmin" {
		t.Errorf("panel = %+v", panelItem)
	}
	for _, opened := range recorder.opened {
		if strings.HasSuffix(opened, ".key") || strings.HasSuffix(opened, ".combined") || strings.HasSuffix(opened, "cakey.pem") {
			t.Errorf("opened %s", opened)
		}
	}

	// letsencrypt=1 in the domain's configuration says the same.
	config := recorder.FS.(fstest.MapFS)["usr/local/directadmin/data/users/alice/domains/alice.example.conf"]
	config.Data = append(slices.Clone(config.Data), []byte("letsencrypt=1\n")...)
	delete(recorder.FS.(fstest.MapFS), "usr/local/directadmin/data/users/alice/domains/alice.example.cert.creation_time")
	items = certificatesByFingerprint(t, collectCertificates(t, NewCertificates(host, nil)))
	if got := stringValue(items[pemFingerprint(t, alice)].ManagedBy.Config); got != "/usr/local/directadmin/data/users/alice/domains/alice.example.conf" {
		t.Errorf("config = %s", got)
	}
}

func TestCertificatesDeltaKeyAndHashRoundTrip(t *testing.T) {
	pki := newTestPKI(t)
	host, _, _, _ := nginxCertbotHost(t, pki, nil)
	collector := NewCertificates(host, nil)
	info := collectCertificates(t, collector)
	if _, err := inventory.Diff(nil, info.Items, CertificateKeyOf); err != nil {
		t.Fatal(err)
	}
	if fields := collector.DeltaKeyFields(); !slices.Equal(fields, []string{"fingerprint"}) {
		t.Fatalf("key fields = %v", fields)
	}
	dataset, err := inventory.NewDataset("certificates", info)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CertificatesInfo
	if err := json.Unmarshal(dataset.Canonical, &decoded); err != nil {
		t.Fatal(err)
	}
	if hash, err := inventory.HashValue(&decoded); err != nil || hash != dataset.Hash {
		t.Fatalf("round trip hash %s (%v), want %s", hash, err, dataset.Hash)
	}

	// A delta against the first collection applies back to the second.
	extra, _ := pki.leaf(t, leafOptions{commonName: "new.example.test", names: []string{"new.example.test"}})
	host.FS.(*readTracker).MapFS["etc/postfix/main.cf"] = mapFile([]byte("smtpd_tls_cert_file = /etc/ssl/new.pem\n"))
	host.FS.(*readTracker).MapFS["etc/ssl/new.pem"] = mapFile(extra)
	second := collectCertificates(t, collector)
	delta, err := inventory.Diff(info.Items, second.Items, CertificateKeyOf)
	if err != nil || len(delta.Added) != 1 || len(delta.Changed) != 1 || len(delta.Removed) != 0 {
		t.Fatalf("delta = %+v (%v)", delta, err)
	}
	applied, err := inventory.Apply(info.Items, delta, CertificateKeyOf)
	if err != nil {
		t.Fatal(err)
	}
	if hash, _ := inventory.HashValue(&CertificatesInfo{Items: applied}); hash != certificatesHash(t, second) {
		t.Fatal("applied delta does not reproduce the hash")
	}
}

func TestCertificatesChangeToken(t *testing.T) {
	pki := newTestPKI(t)
	host, tracker, _, _ := nginxCertbotHost(t, pki, nil)
	collector := NewCertificates(host, nil)
	collectCertificates(t, collector)
	before := collector.ChangeToken()
	if collector.ChangeToken() != before {
		t.Fatal("token is not stable")
	}
	for _, name := range []string{"etc/letsencrypt/renewal/shop.example.test.conf", "etc/ssl/legacy.example.test.pem", "etc/postfix/main.cf"} {
		tracker.MapFS[name].ModTime = time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
		after := collector.ChangeToken()
		if after == before {
			t.Fatalf("changing %s kept the token", name)
		}
		before = after
	}
	tracker.MapFS["root/.acme.sh/new.example.test/new.example.test.conf"] = mapFile(nil)
	if collector.ChangeToken() == before {
		t.Fatal("a new acme.sh domain kept the token")
	}
}

var _ io.Reader = (*trackedFile)(nil)

// TestCertificatesForge recognises the certificates Laravel Forge renews by
// the per-certificate jobs in /etc/cron.d, without reading the job scripts
// under /root.
func TestCertificatesForge(t *testing.T) {
	pki := newTestPKI(t)
	booking, _ := pki.leaf(t, leafOptions{commonName: "booking.example.com", names: []string{"booking.example.com", "www.booking.example.com"}})
	admin, _ := pki.leaf(t, leafOptions{commonName: "admin.status.example.com", names: []string{"admin.status.example.com"}})
	status, _ := pki.leaf(t, leafOptions{commonName: "status.example.com", names: []string{"status.example.com"}})
	tracker := &readTracker{MapFS: vhostFixture(t, "forge", map[string][]byte{
		"usr/sbin/nginx": nil,
		"etc/nginx/ssl/domains/200001/300001/server.crt": booking,
		"etc/nginx/ssl/domains/200002/300002/server.crt": admin,
		// A certificate Forge installed but does not renew.
		"etc/nginx/ssl/domains/200003/300003/server.crt":      status,
		"etc/cron.d/letsencrypt-renew-domain-200001":          []byte("SHELL=/bin/sh\nPATH=/usr/local/sbin:/usr/local/bin:/sbin:/bin:/usr/sbin:/usr/bin\n\n44 19 * * 0 root /bin/bash /root/.letsencrypt-renew/300001\n"),
		"etc/cron.d/letsencrypt-renew-domain-200002":          []byte("43 7 * * 2 root /bin/bash /root/.letsencrypt-renew/300002\n"),
		"root/.letsencrypt-renew/300001":                      []byte("never read"),
		"etc/cron.d/letsencrypt-renew-domain-200003.dpkg-old": []byte("13 9 * * 1 root /bin/bash /root/.letsencrypt-renew/300003\n"),
	})}
	var ran []string
	host := &Host{FS: tracker, Roots: pki.roots(), Command: vhostCommands(t, "forge", []vhostCommand{{prefix: "nginx -V", file: "nginx-V.txt"}}, &ran)}
	vhosts := NewVHosts(host)
	items := certificatesByFingerprint(t, collectCertificates(t, NewCertificates(host, vhosts)))
	if got, want := managerString(items[pemFingerprint(t, booking)].ManagedBy), "forge|300001|<nil>|<nil>|<nil>|<nil>|<nil>|cron|/root/.letsencrypt-renew/300001"; got != want {
		t.Errorf("booking managed_by\n got %s\nwant %s", got, want)
	}
	if got, want := managerString(items[pemFingerprint(t, admin)].ManagedBy), "forge|300002|<nil>|<nil>|<nil>|<nil>|<nil>|cron|/root/.letsencrypt-renew/300002"; got != want {
		t.Errorf("admin managed_by\n got %s\nwant %s", got, want)
	}
	if manager := items[pemFingerprint(t, status)].ManagedBy; manager != nil {
		t.Errorf("status managed_by = %s", managerString(manager))
	}
	for name := range tracker.opens {
		if strings.HasPrefix(name, "root/") {
			t.Errorf("opened %s", name)
		}
	}
}
