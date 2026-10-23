package collector

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io/fs"
	"math/big"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"go.chief.tools/serverchief-agent/inventory"
)

const vhostsTestdata = "testdata/vhosts"

var (
	testCAOnce sync.Once
	testCA     *x509.Certificate
	testCAKey  *ecdsa.PrivateKey
)

// testCertificate issues a certificate from a throwaway test CA ("Test CA")
// and returns the certificate and key PEM. Nothing is committed: every
// test run generates its own keys.
func testCertificate(t *testing.T, commonName string, names []string, notAfter time.Time) (certificatePEM, keyPEM []byte) {
	t.Helper()
	testCAOnce.Do(func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			panic(err)
		}
		template := &x509.Certificate{
			SerialNumber:          big.NewInt(1),
			Subject:               pkix.Name{CommonName: "Test CA"},
			NotBefore:             time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			NotAfter:              time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC),
			IsCA:                  true,
			BasicConstraintsValid: true,
			KeyUsage:              x509.KeyUsageCertSign,
		}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			panic(err)
		}
		testCA, _ = x509.ParseCertificate(der)
		testCAKey = key
	})
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName},
		DNSNames:     names,
		NotBefore:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, testCA, &key.PublicKey, testCAKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pemBlock("CERTIFICATE", der), pemBlock("EC PRIVATE KEY", keyDER)
}

func pemBlock(kind string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
}

// vhostFixture loads testdata/vhosts/<name>/root into a MapFS and adds
// files (for generated certificates and binaries).
func vhostFixture(t *testing.T, name string, files map[string][]byte) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{}
	root := filepath.Join(vhostsTestdata, name, "root")
	if _, err := os.Stat(root); err == nil {
		err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			relativeName, _ := filepath.Rel(root, file)
			fsys[filepath.ToSlash(relativeName)] = &fstest.MapFile{Data: data, ModTime: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range files {
		fsys[strings.TrimPrefix(name, "/")] = &fstest.MapFile{Data: data, ModTime: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	}
	return fsys
}

// openRecorder wraps an fs.FS and records every opened path.
type openRecorder struct {
	fs.FS
	mu     sync.Mutex
	opened []string
}

func (recorder *openRecorder) Open(name string) (fs.File, error) {
	recorder.mu.Lock()
	recorder.opened = append(recorder.opened, name)
	recorder.mu.Unlock()
	return recorder.FS.Open(name)
}

// vhostCommand answers a command whose "basename args…" line starts with
// prefix.
type vhostCommand struct {
	prefix string
	file   string // under testdata/vhosts/<case>/commands
	data   []byte // instead of file
	exit   int
	err    error
}

func vhostCommands(t *testing.T, fixture string, commands []vhostCommand, ran *[]string) func(context.Context, string, ...string) (CommandResult, error) {
	return func(_ context.Context, name string, args ...string) (CommandResult, error) {
		line := strings.Join(append([]string{path.Base(name)}, args...), " ")
		*ran = append(*ran, line)
		for _, command := range commands {
			if !strings.HasPrefix(line, command.prefix) {
				continue
			}
			if command.err != nil {
				return CommandResult{}, command.err
			}
			output := command.data
			if command.file != "" {
				var err error
				output, err = os.ReadFile(filepath.Join(vhostsTestdata, fixture, "commands", command.file))
				if err != nil {
					t.Fatal(err)
				}
			}
			if command.exit != 0 {
				return CommandResult{Stderr: output, ExitCode: command.exit}, nil
			}
			return CommandResult{Stdout: output}, nil
		}
		return CommandResult{}, errors.New("unexpected command: " + line)
	}
}

func collectVHosts(t *testing.T, collector *VHosts) *VHostsInfo {
	t.Helper()
	payload, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info := payload.(*VHostsInfo)
	if !slices.IsSortedFunc(info.Items, func(a, b VHost) int { return strings.Compare(VHostKey(a), VHostKey(b)) }) {
		t.Fatal("items are not sorted by key")
	}
	return info
}

// vhostsJSON returns the items as indented JSON, one item per key, so
// failures show a readable diff.
func vhostsJSON(t *testing.T, info *VHostsInfo) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, item := range info.Items {
		var buffer bytes.Buffer
		encoder := json.NewEncoder(&buffer)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(item); err != nil {
			t.Fatal(err)
		}
		result[VHostKey(item)] = strings.TrimSpace(buffer.String())
	}
	return result
}

func checkVHosts(t *testing.T, info *VHostsInfo, want map[string]string) {
	t.Helper()
	got := vhostsJSON(t, info)
	for key, expected := range want {
		if got[key] != expected {
			t.Errorf("%s\n got %s\nwant %s", key, got[key], expected)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("unexpected item %s: %s", key, got[key])
		}
	}
}

var certificateExpiry = time.Date(2027, 1, 20, 8, 30, 0, 0, time.UTC)

func TestVHostsApacheDebian(t *testing.T) {
	exampleCert, _ := testCertificate(t, "example.com", []string{"www.example.com", "example.com"}, certificateExpiry)
	shopCert, shopKey := testCertificate(t, "shop.example.org", []string{"shop.example.org", "*.shop.example.org"}, certificateExpiry)
	recorder := &openRecorder{FS: vhostFixture(t, "apache-debian", map[string][]byte{
		"usr/sbin/apache2ctl":             nil,
		"etc/ssl/certs/example.com.pem":   exampleCert,
		"etc/ssl/private/example.com.key": []byte("not to be read"),
		// Key first, then the certificate, like Plesk's certificate files.
		"etc/ssl/shop/combined.pem": append(append([]byte{}, shopKey...), shopCert...),
	})}
	var ran []string
	host := &Host{FS: recorder, Command: vhostCommands(t, "apache-debian", []vhostCommand{{prefix: "apache2ctl -S", file: "apache2ctl-S.txt"}}, &ran)}
	info := collectVHosts(t, NewVHosts(host))

	checkVHosts(t, info, map[string]string{
		"apache/example.com":        `{"source":"apache","name":"example.com","aliases":["www.example.com"],"ports":[80,443],"ssl":true,"document_root":"/var/www/example.com/public","php":{"version":"8.2","handler":"fpm"},"certificate":{"subject":"example.com","issuer":"Test CA","not_after":"2027-01-20T08:30:00Z","names":["example.com","www.example.com"]},"config_file":"/etc/apache2/sites-enabled/example.com.conf","owner":null}`,
		"apache/shop.example.org":   `{"source":"apache","name":"shop.example.org","aliases":["*.shop.example.org"],"ports":[443],"ssl":true,"document_root":"/srv/shop/htdocs","php":{"version":"8.2","handler":"module"},"certificate":{"subject":"shop.example.org","issuer":"Test CA","not_after":"2027-01-20T08:30:00Z","names":["*.shop.example.org","shop.example.org"]},"config_file":"/etc/apache2/sites-enabled/shop.example.org.conf","owner":null}`,
		"apache/web-01.example.net": `{"source":"apache","name":"web-01.example.net","aliases":[],"ports":[80],"ssl":false,"document_root":"/var/www/html","php":{"version":"8.2","handler":"module"},"certificate":null,"config_file":"/etc/apache2/sites-enabled/000-default.conf","owner":null}`,
	})
	if !slices.Equal(ran, []string{"apache2ctl -S"}) {
		t.Fatalf("ran %v", ran)
	}
	for _, opened := range recorder.opened {
		if strings.Contains(opened, "private") || strings.HasSuffix(opened, ".key") {
			t.Errorf("opened %s", opened)
		}
	}
}

func TestVHostsApacheEL9(t *testing.T) {
	localhostCert, _ := testCertificate(t, "www.example.net", []string{"www.example.net", "example.net"}, certificateExpiry)
	var ran []string
	host := &Host{
		FS: vhostFixture(t, "apache-el9", map[string][]byte{
			"usr/sbin/httpd":                  nil,
			"usr/sbin/apachectl":              nil,
			"etc/pki/tls/certs/localhost.crt": localhostCert,
		}),
		Command: vhostCommands(t, "apache-el9", []vhostCommand{
			{prefix: "httpd -S", file: "httpd-S.txt"},
			{prefix: "apachectl -S", data: []byte(`apachectl: The "-S" option is not supported.`), exit: 2},
		}, &ran),
	}
	info := collectVHosts(t, NewVHosts(host))
	if !slices.Equal(ran, []string{"httpd -S"}) {
		t.Fatalf("ran %v", ran)
	}
	checkVHosts(t, info, map[string]string{
		"apache/legacy.example.net": `{"source":"apache","name":"legacy.example.net","aliases":[],"ports":[80],"ssl":false,"document_root":"/srv/legacy/public","php":{"version":null,"handler":"fpm"},"certificate":null,"config_file":"/etc/httpd/conf.d/legacy.conf","owner":null}`,
		"apache/www.example.net":    `{"source":"apache","name":"www.example.net","aliases":["example.net"],"ports":[80,443],"ssl":true,"document_root":"/var/www/example.net","php":{"version":"8.3","handler":"fpm"},"certificate":{"subject":"www.example.net","issuer":"Test CA","not_after":"2027-01-20T08:30:00Z","names":["example.net","www.example.net"]},"config_file":"/etc/httpd/conf.d/ssl.conf","owner":null}`,
	})
}

func TestVHostsNginx(t *testing.T) {
	exampleCert, _ := testCertificate(t, "example.com", []string{"example.com", "www.example.com", "*.cdn.example.com"}, certificateExpiry)
	apiCert, _ := testCertificate(t, "api.example.org", []string{"api.example.org"}, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	var ran []string
	host := &Host{
		FS: vhostFixture(t, "nginx", map[string][]byte{
			"usr/sbin/nginx": nil,
			"etc/letsencrypt/live/example.com/fullchain.pem": exampleCert,
			"etc/nginx/ssl/api.example.org.pem":              apiCert,
		}),
		Command: vhostCommands(t, "nginx", []vhostCommand{{prefix: "nginx -T", file: "nginx-T.txt"}}, &ran),
	}
	info := collectVHosts(t, NewVHosts(host))
	checkVHosts(t, info, map[string]string{
		"nginx/api.example.org":    `{"source":"nginx","name":"api.example.org","aliases":["~^api-(?<region>[a-z]+)\\.example\\.org$"],"ports":[8080,8443],"ssl":true,"document_root":"/srv/api/public","php":{"version":"8.2","handler":"fpm"},"certificate":{"subject":"api.example.org","issuer":"Test CA","not_after":"2026-12-01T00:00:00Z","names":["api.example.org"]},"config_file":"/etc/nginx/sites-enabled/api.example.org","owner":null}`,
		"nginx/example.com":        `{"source":"nginx","name":"example.com","aliases":["*.cdn.example.com","www.example.com"],"ports":[80,443],"ssl":true,"document_root":"/var/www/example.com/public","php":{"version":"8.2","handler":"fpm"},"certificate":{"subject":"example.com","issuer":"Test CA","not_after":"2027-01-20T08:30:00Z","names":["*.cdn.example.com","example.com","www.example.com"]},"config_file":"/etc/nginx/sites-enabled/example.com","owner":null}`,
		"nginx/legacy.example.net": `{"source":"nginx","name":"legacy.example.net","aliases":[],"ports":[80],"ssl":false,"document_root":"/srv/legacy","php":{"version":null,"handler":"fpm"},"certificate":null,"config_file":"/etc/nginx/sites-enabled/legacy","owner":null}`,
	})
}

func pleskHost(t *testing.T, commands []vhostCommand, ran *[]string) *Host {
	t.Helper()
	return &Host{
		FS: vhostFixture(t, "plesk", map[string][]byte{
			"usr/sbin/plesk":        nil,
			"usr/local/psa/version": []byte("18.0.74 Debian 12.0 1800251015.10\n"),
			"var/www/vhosts/system/example.com/conf/httpd.conf": []byte("# generated by Plesk\n"),
		}),
		Command: vhostCommands(t, "plesk", commands, ran),
	}
}

func TestVHostsPlesk(t *testing.T) {
	certificate, _ := testCertificate(t, "example.com", []string{"example.com", "www.example.com", "example.net"}, certificateExpiry)
	domains, err := os.ReadFile(filepath.Join(vhostsTestdata, "plesk", "commands", "domains.txt"))
	if err != nil {
		t.Fatal(err)
	}
	domains = bytes.ReplaceAll(domains, []byte("@CERT_HEX@"), []byte(strings.ToUpper(hex.EncodeToString(certificate))))
	var ran []string
	host := pleskHost(t, []vhostCommand{
		{prefix: "plesk db -Ne SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.COLUMNS", file: "columns.txt"},
		{prefix: "plesk db -Ne SELECT d.id, d.name", data: domains},
		{prefix: "plesk db -Ne SELECT dom_id, name FROM domain_aliases", file: "aliases.txt"},
	}, &ran)
	info := collectVHosts(t, NewVHosts(host))
	checkVHosts(t, info, map[string]string{
		"plesk/customer.nl":           `{"source":"plesk","name":"customer.nl","aliases":["customer.be"],"ports":[80],"ssl":false,"document_root":"/var/www/vhosts/customer.nl/httpdocs","php":null,"certificate":null,"config_file":null,"owner":"customer.nl"}`,
		"plesk/example.com":           `{"source":"plesk","name":"example.com","aliases":["example.net","www.example.net"],"ports":[80,443],"ssl":true,"document_root":"/var/www/vhosts/example.com/httpdocs","php":{"version":"8.2","handler":"fpm"},"certificate":{"subject":"example.com","issuer":"Test CA","not_after":"2027-01-20T08:30:00Z","names":["example.com","example.net","www.example.com"]},"config_file":"/var/www/vhosts/system/example.com/conf/httpd.conf","owner":"example.com"}`,
		"plesk/shop.example.com":      `{"source":"plesk","name":"shop.example.com","aliases":[],"ports":[80],"ssl":false,"document_root":"/var/www/vhosts/example.com/shop.example.com","php":{"version":"7.4","handler":"fastcgi"},"certificate":null,"config_file":null,"owner":"example.com"}`,
		"plesk/static.customer.nl":    `{"source":"plesk","name":"static.customer.nl","aliases":[],"ports":[80,443],"ssl":true,"document_root":"/var/www/vhosts/customer.nl/static","php":{"version":null,"handler":"fpm"},"certificate":null,"config_file":null,"owner":"customer.nl"}`,
		"plesk/xn--bcher-kva.example": `{"source":"plesk","name":"xn--bcher-kva.example","aliases":[],"ports":[80],"ssl":false,"document_root":"/var/www/vhosts/xn--bcher-kva.example/httpdocs","php":{"version":"8.1","handler":"cgi"},"certificate":null,"config_file":null,"owner":"xn--bcher-kva.example"}`,
	})
	if len(ran) != 3 {
		t.Fatalf("ran %v", ran)
	}
	for _, command := range ran {
		upper := strings.ToUpper(command)
		if strings.Contains(upper, "PVT_KEY") || strings.Contains(upper, "CSR") {
			t.Errorf("query selects a private column: %s", command)
		}
		for _, verb := range []string{"UPDATE ", "INSERT ", "DELETE ", "REPLACE ", "ALTER ", "DROP "} {
			if strings.Contains(upper, verb) {
				t.Errorf("query is not read-only: %s", command)
			}
		}
	}
}

func TestPleskQueryWithoutOptionalColumns(t *testing.T) {
	columns := pleskColumns{
		"domains":        {"id": true, "name": true},
		"hosting":        {"dom_id": true, "www_root": true},
		"domain_aliases": {"dom_id": true, "name": true},
	}
	query, err := pleskDomainsQuery(columns)
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT d.id, d.name, d.name, h.www_root, NULL, NULL, NULL, NULL FROM domains d JOIN hosting h ON h.dom_id = d.id ORDER BY d.id"
	if query != want {
		t.Fatalf("query\n got %s\nwant %s", query, want)
	}
	if got := pleskAliasesQuery(columns); got != "SELECT dom_id, name FROM domain_aliases ORDER BY dom_id, name" {
		t.Fatalf("aliases query = %s", got)
	}
	if _, err := pleskDomainsQuery(pleskColumns{"domains": {"id": true}}); err == nil {
		t.Fatal("query without domains.name")
	}
}

func TestVHostsPleskFallsBackToSiteList(t *testing.T) {
	var ran []string
	host := pleskHost(t, []vhostCommand{
		{prefix: "plesk db", data: []byte("ERROR 1045 (28000): Access denied for user 'admin'@'localhost'\n"), exit: 1},
		{prefix: "plesk bin site --list", file: "site-list.txt"},
	}, &ran)
	info := collectVHosts(t, NewVHosts(host))
	if len(info.Items) != 5 || info.Items[0].Name != "customer.nl" || info.Items[0].Owner != nil || len(info.Items[0].Ports) != 0 {
		t.Fatalf("items = %v", vhostsJSON(t, info))
	}
}

func TestVHostsDirectAdmin(t *testing.T) {
	certificate, _ := testCertificate(t, "alice.example", []string{"alice.example", "www.alice.example", "blog.alice.example"}, certificateExpiry)
	host := &Host{FS: vhostFixture(t, "directadmin", map[string][]byte{
		"usr/local/directadmin/data/users/alice/domains/alice.example.cert": certificate,
		"usr/local/directadmin/data/users/alice/domains/alice.example.key":  []byte("not to be read"),
	})}
	info := collectVHosts(t, NewVHosts(host))
	aliceCert := `{"subject":"alice.example","issuer":"Test CA","not_after":"2027-01-20T08:30:00Z","names":["alice.example","blog.alice.example","www.alice.example"]}`
	aliceConf := `"/usr/local/directadmin/data/users/alice/domains/alice.example.conf"`
	checkVHosts(t, info, map[string]string{
		"directadmin/alice.example":         `{"source":"directadmin","name":"alice.example","aliases":["alice.example.net"],"ports":[80,443],"ssl":true,"document_root":"/home/alice/domains/alice.example/public_html","php":{"version":"7.4","handler":"lsapi"},"certificate":` + aliceCert + `,"config_file":` + aliceConf + `,"owner":"alice"}`,
		"directadmin/blog.alice.example":    `{"source":"directadmin","name":"blog.alice.example","aliases":[],"ports":[80,443],"ssl":true,"document_root":"/home/alice/domains/alice.example/public_html/blog","php":{"version":"7.4","handler":"lsapi"},"certificate":` + aliceCert + `,"config_file":` + aliceConf + `,"owner":"alice"}`,
		"directadmin/staging.alice.example": `{"source":"directadmin","name":"staging.alice.example","aliases":[],"ports":[80,443],"ssl":true,"document_root":"/home/alice/domains/alice.example/public_html/staging","php":{"version":"7.4","handler":"lsapi"},"certificate":` + aliceCert + `,"config_file":` + aliceConf + `,"owner":"alice"}`,
		"directadmin/shop.alice.example":    `{"source":"directadmin","name":"shop.alice.example","aliases":[],"ports":[80],"ssl":false,"document_root":"/home/alice/domains/shop.alice.example/public_html","php":null,"certificate":null,"config_file":"/usr/local/directadmin/data/users/alice/domains/shop.alice.example.conf","owner":"alice"}`,
		"directadmin/bob.example":           `{"source":"directadmin","name":"bob.example","aliases":[],"ports":[80],"ssl":false,"document_root":"/srv/home/bob/domains/bob.example/public_html","php":{"version":"8.2","handler":"fpm"},"certificate":null,"config_file":"/usr/local/directadmin/data/users/bob/domains/bob.example.conf","owner":"bob"}`,
	})
}

// combinedHost has Apache (Debian fixture) and nginx; nginx answers with
// nginxResult.
func combinedHost(t *testing.T, nginx vhostCommand) *Host {
	t.Helper()
	files := vhostFixture(t, "apache-debian", map[string][]byte{"usr/sbin/apache2ctl": nil, "usr/sbin/nginx": nil})
	var ran []string
	nginx.prefix = "nginx -T"
	return &Host{FS: files, Command: vhostCommands(t, "nginx", []vhostCommand{
		{prefix: "apache2ctl -S", data: mustRead(t, filepath.Join(vhostsTestdata, "apache-debian", "commands", "apache2ctl-S.txt"))},
		nginx,
	}, &ran)}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func countSource(info *VHostsInfo, source string) int {
	count := 0
	for _, item := range info.Items {
		if item.Source == source {
			count++
		}
	}
	return count
}

func TestVHostsPartialFailure(t *testing.T) {
	t.Run("one source fails", func(t *testing.T) {
		host := combinedHost(t, vhostCommand{data: []byte("nginx: [emerg] unknown directive \"sever\" in /etc/nginx/sites-enabled/x:1\n"), exit: 1})
		info := collectVHosts(t, NewVHosts(host))
		if countSource(info, VHostSourceApache) != 3 || countSource(info, VHostSourceNginx) != 0 {
			t.Fatalf("items = %v", vhostsJSON(t, info))
		}
	})

	t.Run("every source fails", func(t *testing.T) {
		host := combinedHost(t, vhostCommand{err: errors.New("exec: permission denied")})
		host.Command = func(_ context.Context, name string, args ...string) (CommandResult, error) {
			return CommandResult{}, errors.New("exec: permission denied")
		}
		_, err := NewVHosts(host).Collect(context.Background())
		if err == nil || !strings.Contains(err.Error(), "apache: apache2ctl -S: exec: permission denied") || !strings.Contains(err.Error(), "nginx: nginx -T: exec: permission denied") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("a failed source keeps its previous items", func(t *testing.T) {
		host := combinedHost(t, vhostCommand{file: "nginx-T.txt"})
		collector := NewVHosts(host)
		first := collectVHosts(t, collector)
		if countSource(first, VHostSourceNginx) != 3 || countSource(first, VHostSourceApache) != 3 {
			t.Fatalf("first = %v", vhostsJSON(t, first))
		}
		broken := combinedHost(t, vhostCommand{data: []byte("nginx: [emerg] bad\n"), exit: 1})
		collector.host = broken
		second := collectVHosts(t, collector)
		if a, b := inventoryHash(t, first), inventoryHash(t, second); a != b {
			t.Fatalf("payload changed after a failed source: %s != %s\n%v\n%v", a, b, vhostsJSON(t, first), vhostsJSON(t, second))
		}
	})

	t.Run("no sources", func(t *testing.T) {
		info := collectVHosts(t, NewVHosts(&Host{FS: fstest.MapFS{}}))
		if data, _ := json.Marshal(info); string(data) != `{"items":[]}` {
			t.Fatalf("payload = %s", data)
		}
	})
}

func inventoryHash(t *testing.T, info *VHostsInfo) string {
	t.Helper()
	dataset, err := inventory.NewDataset("vhosts", info)
	if err != nil {
		t.Fatal(err)
	}
	return dataset.Hash
}

func TestVHostsChangeToken(t *testing.T) {
	fsys := fstest.MapFS{
		"etc/nginx/nginx.conf":                                      {Data: []byte("x"), ModTime: time.Unix(100, 0)},
		"etc/nginx/sites-enabled/example.com":                       {Data: []byte("x"), ModTime: time.Unix(100, 0)},
		"usr/local/directadmin/data/users/a/domains/a.example.conf": {Data: []byte("x"), ModTime: time.Unix(100, 0)},
	}
	collector := NewVHosts(&Host{FS: fsys})
	before := collector.ChangeToken()
	if collector.ChangeToken() != before {
		t.Fatal("token is not stable")
	}
	for _, name := range []string{"etc/nginx/sites-enabled/example.com", "usr/local/directadmin/data/users/a/domains/a.example.conf"} {
		fsys[name].ModTime = time.Unix(200, 0)
		after := collector.ChangeToken()
		if after == before {
			t.Fatalf("changing %s kept the token", name)
		}
		before = after
	}
	fsys["etc/nginx/sites-enabled/new.example"] = &fstest.MapFile{Data: []byte("x"), ModTime: time.Unix(100, 0)}
	if collector.ChangeToken() == before {
		t.Fatal("a new site kept the token")
	}
}

func TestReadCertificateFile(t *testing.T) {
	certificate, key := testCertificate(t, "Example.COM", []string{"WWW.example.com", "example.com", "example.com"}, certificateExpiry)
	other, _ := testCertificate(t, "intermediate.example", nil, certificateExpiry)
	host := &Host{FS: fstest.MapFS{
		"chain.pem":    {Data: append(append(append([]byte{}, key...), certificate...), other...)},
		"key-only.pem": {Data: key},
		"garbage.pem":  {Data: []byte("-----BEGIN CERTIFICATE-----\nnot base64\n-----END CERTIFICATE-----\n")},
	}}
	got := host.readCertificateFile("/chain.pem")
	if got == nil || got.Subject != "Example.COM" || !slices.Equal(got.Names, []string{"example.com", "www.example.com"}) {
		t.Fatalf("chain.pem = %+v", got)
	}
	for _, name := range []string{"/key-only.pem", "/garbage.pem", "/missing.pem"} {
		if got := host.readCertificateFile(name); got != nil {
			t.Errorf("%s = %+v", name, got)
		}
	}
}

func TestPHPVersionFrom(t *testing.T) {
	cases := map[string]string{
		"unix:/run/php/php8.2-fpm.sock":                  "8.2",
		"/var/opt/remi/php83/run/php-fpm/www.sock":       "8.3",
		"plesk-php74-fastcgi":                            "7.4",
		"/opt/cpanel/ea-php81/root/usr/var/run/php-fpm/": "8.1",
		"/usr/local/php56/bin/php-cgi":                   "5.6",
		"/run/php-fpm/www.sock":                          "",
		"127.0.0.1:9000":                                 "",
		"php8.10":                                        "8.10",
	}
	for value, want := range cases {
		got := phpVersionFrom(value)
		if (got == nil) != (want == "") || got != nil && *got != want {
			t.Errorf("phpVersionFrom(%q) = %v, want %q", value, got, want)
		}
	}
}

func TestNginxListenPort(t *testing.T) {
	cases := map[string]int{"80": 80, "[::]:443": 443, "127.0.0.1:8080": 8080, "*:8000": 8000, "[::1]": 80, "192.0.2.1": 80, "localhost": 80, "unix:/run/nginx.sock": 0, "abc": 0}
	for address, want := range cases {
		got, ok := nginxListenPort(address)
		if got != want || ok != (want != 0) {
			t.Errorf("nginxListenPort(%q) = %d, %v", address, got, ok)
		}
	}
}

func TestParseApacheDumpSingleVHostAndIPv6(t *testing.T) {
	dump := parseApacheDump([]byte(`VirtualHost configuration:
[2001:db8::10]:443     secure.example.com (/etc/apache2/sites-enabled/secure.conf:3)
*:80                   is a NameVirtualHost
         port 80 namevhost fd07:b51a:cc66:1::6 (/etc/apache2/sites-enabled/000-default.conf:1)
         port 80 namevhost 172.17.0.2 (/etc/apache2/sites-enabled/000-default.conf:1)
192.0.2.10:80          is a NameVirtualHost
         default server a.example (/etc/apache2/sites-enabled/a.conf:1)
         port 80 namevhost a.example (/etc/apache2/sites-enabled/a.conf:1)
                 alias WWW.A.example
ServerRoot: "/etc/apache2"
`))
	want := []apacheVHostRef{
		{port: 443, name: "secure.example.com", file: "/etc/apache2/sites-enabled/secure.conf", line: 3},
		{port: 80, name: "a.example", aliases: []string{"www.a.example"}, file: "/etc/apache2/sites-enabled/a.conf", line: 1},
	}
	if dump.serverRoot != "/etc/apache2" || len(dump.vhosts) != 2 {
		t.Fatalf("dump = %+v", dump)
	}
	for index := range want {
		got := dump.vhosts[index]
		if got.port != want[index].port || got.name != want[index].name || got.file != want[index].file || got.line != want[index].line || !slices.Equal(got.aliases, want[index].aliases) {
			t.Errorf("vhost %d = %+v, want %+v", index, got, want[index])
		}
	}
}

func TestVHostsDeltaKeyIsUnique(t *testing.T) {
	host := combinedHost(t, vhostCommand{file: "nginx-T.txt"})
	info := collectVHosts(t, NewVHosts(host))
	_, err := inventory.Diff(nil, info.Items, VHostKey)
	if err != nil {
		t.Fatal(err)
	}
}

func TestVHostsApacheFallsBackToNextControl(t *testing.T) {
	var ran []string
	host := &Host{
		FS: vhostFixture(t, "apache-el9", map[string][]byte{"usr/sbin/httpd": nil, "usr/sbin/apachectl": nil}),
		Command: vhostCommands(t, "apache-el9", []vhostCommand{
			{prefix: "httpd -S", err: errors.New("exec format error")},
			{prefix: "apachectl -S", file: "httpd-S.txt"},
		}, &ran),
	}
	info := collectVHosts(t, NewVHosts(host))
	if len(info.Items) != 2 || !slices.Equal(ran, []string{"httpd -S", "apachectl -S"}) {
		t.Fatalf("items %v, ran %v", vhostsJSON(t, info), ran)
	}
}
