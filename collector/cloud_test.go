package collector

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

const cloudTestdata = "testdata/cloud"

// metadataServer serves testdata/cloud/<provider> as a metadata service:
// GET for leaves, PUT for token paths, and the provider's required headers.
type metadataServer struct {
	*httptest.Server
	mu        sync.Mutex
	requested []string
}

type metadataSpec struct {
	provider string
	// headers must be present on every GET.
	headers map[string]string
}

func newMetadataServer(t *testing.T, spec metadataSpec) *metadataServer {
	t.Helper()
	server := &metadataServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		server.mu.Lock()
		server.requested = append(server.requested, request.Method+" "+request.URL.Path)
		server.mu.Unlock()
		if request.Header.Get("X-Forwarded-For") != "" || request.Header.Get("Via") != "" {
			http.Error(writer, "proxied", http.StatusBadRequest)
			return
		}
		token := strings.HasSuffix(request.URL.Path, "/api/token") || request.URL.Path == "/v1/token"
		if token != (request.Method == http.MethodPut) {
			http.Error(writer, "method", http.StatusMethodNotAllowed)
			return
		}
		if !token {
			for name, value := range spec.headers {
				if request.Header.Get(name) != value {
					http.Error(writer, "missing "+name, http.StatusUnauthorized)
					return
				}
			}
		}
		file := filepath.Join(cloudTestdata, spec.provider, filepath.FromSlash(strings.TrimPrefix(request.URL.Path, "/")))
		info, err := os.Stat(file)
		if err != nil || info.IsDir() {
			http.NotFound(writer, request)
			return
		}
		http.ServeFile(writer, request, file)
	}))
	t.Cleanup(server.Close)
	return server
}

func (server *metadataServer) paths() []string {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]string(nil), server.requested...)
}

func testMetadata(url string) *Metadata {
	client := metadataHTTPClient(nil)
	return &Metadata{Client: client, ScalewayClient: client, LinkLocalURL: url, GoogleURL: url, ScalewayURL: url, TransIPURL: url + "/cloudinit", ProbeTimeout: 2 * time.Second, Timeout: 5 * time.Second}
}

func dmiHost(dmi map[string]string) *Host {
	fsys := fstest.MapFS{}
	for name, value := range dmi {
		fsys["sys/class/dmi/id/"+name] = &fstest.MapFile{Data: []byte(value + "\n")}
	}
	return &Host{FS: fsys}
}

func collectCloud(t *testing.T, host *Host) CloudInfo {
	t.Helper()
	payload, err := NewCloud(host).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return *payload.(*CloudInfo)
}

func cloudJSON(t *testing.T, info CloudInfo) string {
	t.Helper()
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCloudProviders(t *testing.T) {
	cases := []struct {
		name    string
		dmi     map[string]string
		spec    metadataSpec
		want    string
		mustNot []string
	}{
		{
			name: "hetzner",
			dmi:  map[string]string{"sys_vendor": "Hetzner", "product_name": "vServer", "bios_vendor": "Hetzner"},
			spec: metadataSpec{provider: "hetzner"},
			want: `{"provider":"hetzner","source":"metadata","region":"fsn1","zone":"fsn1-dc14","instance_type":null,"instance_id":"42","public_ipv4":"116.203.0.10","public_ipv6":null}`,
		},
		{
			name:    "aws",
			dmi:     map[string]string{"sys_vendor": "Amazon EC2", "product_name": "t3.micro", "bios_vendor": "Amazon EC2", "chassis_asset_tag": "Amazon EC2"},
			spec:    metadataSpec{provider: "aws", headers: map[string]string{"X-aws-ec2-metadata-token": "AQAEAExampleSessionToken=="}},
			want:    `{"provider":"aws","source":"metadata","region":"eu-central-1","zone":"eu-central-1a","instance_type":"t3.micro","instance_id":"i-0123456789abcdef0","public_ipv4":"3.120.0.10","public_ipv6":null}`,
			mustNot: []string{"user-data", "iam", "security-credentials"},
		},
		{
			name:    "digitalocean",
			dmi:     map[string]string{"sys_vendor": "DigitalOcean", "product_name": "Droplet", "bios_vendor": "DigitalOcean"},
			spec:    metadataSpec{provider: "digitalocean"},
			want:    `{"provider":"digitalocean","source":"metadata","region":"ams3","zone":null,"instance_type":null,"instance_id":"2756294","public_ipv4":"188.166.0.10","public_ipv6":"2a03:b0c0:2:d0::1a:2001"}`,
			mustNot: []string{"v1.json", "user-data"},
		},
		{
			name: "gcp",
			dmi:  map[string]string{"sys_vendor": "Google", "product_name": "Google Compute Engine", "bios_vendor": "Google"},
			spec: metadataSpec{provider: "gcp", headers: map[string]string{"Metadata-Flavor": "Google"}},
			want: `{"provider":"gcp","source":"metadata","region":"europe-west4","zone":"europe-west4-a","instance_type":"e2-medium","instance_id":"4567890123456789012","public_ipv4":"34.90.0.10","public_ipv6":null}`,
		},
		{
			name: "azure",
			dmi:  map[string]string{"sys_vendor": "Microsoft Corporation", "product_name": "Virtual Machine", "chassis_asset_tag": azureAssetTag},
			spec: metadataSpec{provider: "azure", headers: map[string]string{"Metadata": "true"}},
			want: `{"provider":"azure","source":"metadata","region":"westeurope","zone":"1","instance_type":"Standard_B2s","instance_id":"02aab8a4-74ef-476e-8182-f6d2ba4166a6","public_ipv4":"20.50.0.10","public_ipv6":null}`,
		},
		{
			name:    "ovh",
			dmi:     map[string]string{"sys_vendor": "OpenStack Foundation", "product_name": "OpenStack Nova"},
			spec:    metadataSpec{provider: "ovh"},
			want:    `{"provider":"ovh","source":"metadata","region":null,"zone":null,"instance_type":"d2-2","instance_id":"i-00a1b2c3","public_ipv4":"51.75.0.10","public_ipv6":null}`,
			mustNot: []string{"meta_data.json", "user_data", "password"},
		},
		{
			name:    "openstack",
			dmi:     map[string]string{"sys_vendor": "OpenStack Foundation", "product_name": "OpenStack Nova"},
			spec:    metadataSpec{provider: "openstack"},
			want:    `{"provider":"openstack","source":"metadata","region":null,"zone":null,"instance_type":"m1.small","instance_id":"i-0000001a","public_ipv4":null,"public_ipv6":null}`,
			mustNot: []string{"meta_data.json", "user_data"},
		},
		{
			name: "oracle",
			dmi:  map[string]string{"sys_vendor": "QEMU", "product_name": "Standard PC (i440FX + PIIX, 1996)", "chassis_asset_tag": "OracleCloud.com"},
			spec: metadataSpec{provider: "oracle", headers: map[string]string{"Authorization": "Bearer Oracle"}},
			want: `{"provider":"oracle","source":"metadata","region":"eu-frankfurt-1","zone":"Uocm:EU-FRANKFURT-1-AD-1","instance_type":"VM.Standard.A1.Flex","instance_id":"ocid1.instance.oc1.eu-frankfurt-1.antheljtexampleinstance","public_ipv4":null,"public_ipv6":null}`,
		},
		{
			name: "vultr",
			dmi:  map[string]string{"sys_vendor": "Vultr", "product_name": "VC2"},
			spec: metadataSpec{provider: "vultr"},
			want: `{"provider":"vultr","source":"metadata","region":"ams","zone":null,"instance_type":null,"instance_id":"a747bf06-385a-46f6-b7fe-6beb2a3a1b52","public_ipv4":"45.76.0.10","public_ipv6":"2001:19f0:5001:1234:5400:3ff:fe12:3456"}`,
		},
		{
			name: "linode",
			dmi:  map[string]string{"sys_vendor": "Linode", "product_name": "Compute Instance"},
			spec: metadataSpec{provider: "linode", headers: map[string]string{"Metadata-Token": "exampleLinodeMetadataToken"}},
			want: `{"provider":"linode","source":"metadata","region":"nl-ams","zone":null,"instance_type":"g6-nanode-1","instance_id":"12345678","public_ipv4":"172.105.0.10","public_ipv6":"2a01:7e01::f03c:94ff:fe12:3456"}`,
		},
		{
			name: "scaleway",
			dmi:  map[string]string{"sys_vendor": "Scaleway", "product_name": "SCW-DEV1-S"},
			spec: metadataSpec{provider: "scaleway"},
			want: `{"provider":"scaleway","source":"metadata","region":"fr-par","zone":"fr-par-1","instance_type":"DEV1-S","instance_id":"5d8b1f6c-3b2a-4c1d-9e8f-7a6b5c4d3e2f","public_ipv4":"51.15.0.10","public_ipv6":null}`,
		},
		{
			name: "transip",
			dmi:  map[string]string{"sys_vendor": "QEMU", "product_name": "Standard PC (i440FX + PIIX, 1996)", "product_serial": "ds=nocloud-net;s=http://pxe.frontend.transip.nl/cloudinit/"},
			spec: metadataSpec{provider: "transip"},
			want: `{"provider":"transip","source":"metadata","region":"rtm0","zone":"rtm0","instance_type":null,"instance_id":"0a1b2c3d-4e5f-6071-8293-0000a4b5c6d7","public_ipv4":null,"public_ipv6":null}`,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := newMetadataServer(t, test.spec)
			host := dmiHost(test.dmi)
			host.Metadata = testMetadata(server.URL)
			if got := cloudJSON(t, collectCloud(t, host)); got != test.want {
				t.Errorf("payload\n got %s\nwant %s", got, test.want)
			}
			for _, requested := range server.paths() {
				lower := strings.ToLower(requested)
				for _, forbidden := range append(test.mustNot, "user-data", "user_data", "userdata", "vendor", "credentials") {
					if strings.Contains(lower, forbidden) {
						t.Errorf("requested %s", requested)
					}
				}
			}
		})
	}
}

func TestCloudProbeWhenDMIIsInconclusive(t *testing.T) {
	server := newMetadataServer(t, metadataSpec{provider: "openstack"})
	host := dmiHost(map[string]string{"sys_vendor": "QEMU", "product_name": "Standard PC (Q35 + ICH9, 2009)"})
	host.Metadata = testMetadata(server.URL)
	info := collectCloud(t, host)
	if info.Provider == nil || *info.Provider != CloudOpenStack || *info.Source != CloudSourceMetadata || *info.InstanceType != "m1.small" {
		t.Fatalf("info = %s", cloudJSON(t, info))
	}
	// The probe tries the providers whose DMI can be inconclusive, in order.
	want := []string{"GET /hetzner/v1/metadata", "PUT /latest/api/token", "GET /metadata/v1/id", "PUT /v1/token", "GET /openstack"}
	if got := server.paths(); len(got) < len(want) || strings.Join(got[:len(want)], ",") != strings.Join(want, ",") {
		t.Fatalf("requests = %v", got)
	}
}

func TestCloudNothingDetected(t *testing.T) {
	nulls := `{"provider":null,"source":null,"region":null,"zone":null,"instance_type":null,"instance_id":null,"public_ipv4":null,"public_ipv6":null}`

	t.Run("no metadata service", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		url := "http://" + listener.Addr().String()
		listener.Close()
		host := dmiHost(map[string]string{"sys_vendor": "Dell Inc.", "product_name": "PowerEdge R640"})
		host.Metadata = testMetadata(url)
		start := time.Now()
		if got := cloudJSON(t, collectCloud(t, host)); got != nulls {
			t.Fatalf("payload = %s", got)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("took %s", elapsed)
		}
	})

	t.Run("something answers, but no known service", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		defer server.Close()
		host := dmiHost(nil)
		host.Metadata = testMetadata(server.URL)
		if got := cloudJSON(t, collectCloud(t, host)); got != nulls {
			t.Fatalf("payload = %s", got)
		}
	})

	t.Run("hanging service is bounded by the probe timeout", func(t *testing.T) {
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
		defer server.Close()
		defer close(release)
		host := dmiHost(nil)
		host.Metadata = testMetadata(server.URL)
		host.Metadata.ProbeTimeout = 300 * time.Millisecond
		start := time.Now()
		if got := cloudJSON(t, collectCloud(t, host)); got != nulls {
			t.Fatalf("payload = %s", got)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("took %s", elapsed)
		}
	})

	t.Run("fixture root without metadata", func(t *testing.T) {
		if got := cloudJSON(t, collectCloud(t, dmiHost(nil))); got != nulls {
			t.Fatalf("payload = %s", got)
		}
	})
}

func TestCloudDMIOnly(t *testing.T) {
	want := `{"provider":"hetzner","source":"dmi","region":null,"zone":null,"instance_type":null,"instance_id":null,"public_ipv4":null,"public_ipv6":null}`
	host := dmiHost(map[string]string{"sys_vendor": "Hetzner", "product_name": "vServer"})
	if got := cloudJSON(t, collectCloud(t, host)); got != want {
		t.Fatalf("without metadata: %s", got)
	}
	// A metadata service that fails keeps the DMI answer.
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	host.Metadata = testMetadata(server.URL)
	if got := cloudJSON(t, collectCloud(t, host)); got != want {
		t.Fatalf("failing metadata: %s", got)
	}
}

func TestCloudFromDMI(t *testing.T) {
	cases := []struct {
		dmi  map[string]string
		uuid string
		want string
	}{
		{map[string]string{"sys_vendor": "Microsoft Corporation", "product_name": "Virtual Machine"}, "", ""}, // Hyper-V outside Azure
		{map[string]string{"sys_vendor": "Xen", "product_name": "HVM domU", "bios_vendor": "Xen"}, "ec2e1916-9099-7caf-fd21-012345abcdef", CloudAWS},
		{map[string]string{"sys_vendor": "QEMU", "product_name": "Standard PC (Q35 + ICH9, 2009)"}, "", ""},
		{map[string]string{"sys_vendor": "OVH", "product_name": "OpenStack Nova"}, "", CloudOVH},
		{map[string]string{"sys_vendor": "ASUSTeK COMPUTER INC.", "board_vendor": "ASUSTeK COMPUTER INC."}, "", ""}, // Hetzner dedicated
		{map[string]string{"sys_vendor": "QEMU", "product_serial": "ds=nocloud-net;s=http://pxe.frontend.transip.nl/cloudinit/"}, "", CloudTransIP},
		{map[string]string{"sys_vendor": "QEMU", "product_serial": "ds=nocloud-net;s=http://seed.example.com/pxe.frontend.transip.nl/"}, "", ""},
	}
	for _, test := range cases {
		host := dmiHost(test.dmi)
		if test.uuid != "" {
			host.FS.(fstest.MapFS)["sys/hypervisor/uuid"] = &fstest.MapFile{Data: []byte(test.uuid + "\n")}
		}
		if got := host.cloudFromDMI(); got != test.want {
			t.Errorf("%v = %q, want %q", test.dmi, got, test.want)
		}
	}
}

// The unprivileged agent cannot read the DMI serial, the datasource cloud-init
// recorded names the TransIP seed instead. Only TransIP's own response with
// the instance counts: anywhere else the seed answers without it.
func TestCloudTransIPFromDatasource(t *testing.T) {
	host := dmiHost(map[string]string{"sys_vendor": "QEMU", "product_name": "Standard PC (i440FX + PIIX, 1996)"})
	host.FS.(fstest.MapFS)["var/lib/cloud/instance/datasource"] = &fstest.MapFile{Data: []byte("DataSourceNoCloudNet: DataSourceNoCloudNet [seed=dmi,http://pxe.frontend.transip.nl/cloudinit/]\n")}
	server := newMetadataServer(t, metadataSpec{provider: "transip"})
	host.Metadata = testMetadata(server.URL)
	if info := collectCloud(t, host); info.Zone == nil || *info.Zone != "rtm0" {
		t.Fatalf("info = %s", cloudJSON(t, info))
	}
	if paths := server.paths(); len(paths) != 1 || paths[0] != "GET /cloudinit/meta-data" {
		t.Fatalf("requested %v", paths)
	}

	other := newMetadataServer(t, metadataSpec{provider: "transip-other"})
	host.Metadata = testMetadata(other.URL)
	want := `{"provider":"transip","source":"dmi","region":null,"zone":null,"instance_type":null,"instance_id":null,"public_ipv4":null,"public_ipv6":null}`
	if got := cloudJSON(t, collectCloud(t, host)); got != want {
		t.Fatalf("seed without the instance: %s", got)
	}
}

// Images from before cloud-init carry no seed. TransIP's apt mirror or
// resolvers on a QEMU guest only lead to asking the seed, which confirms the
// instance; without it nothing is reported.
func TestCloudTransIPFromHints(t *testing.T) {
	cases := []struct {
		name     string
		vendor   string
		files    map[string]string
		seed     string
		provider string
		asked    bool
	}{
		{"apt mirror", "QEMU", map[string]string{"etc/apt/sources.list": "deb http://mirror.transip.net/ubuntu/ubuntu xenial main restricted\n"}, "transip", "transip", true},
		{"resolvers", "QEMU", map[string]string{"etc/resolv.conf": "nameserver 195.8.195.8\nnameserver 195.135.195.135\n"}, "transip", "transip", true},
		{"mirror elsewhere", "QEMU", map[string]string{"etc/apt/sources.list": "deb http://mirror.transip.net/ubuntu/ubuntu xenial main\n"}, "transip-other", "", true},
		{"not qemu", "VMware, Inc.", map[string]string{"etc/apt/sources.list": "deb http://mirror.transip.net/ubuntu/ubuntu xenial main\n"}, "transip", "", false},
		{"no hints", "QEMU", map[string]string{"etc/apt/sources.list": "deb http://archive.ubuntu.com/ubuntu jammy main\n"}, "transip", "", false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := newMetadataServer(t, metadataSpec{provider: test.seed})
			host := dmiHost(map[string]string{"sys_vendor": test.vendor, "product_name": "Standard PC (i440FX + PIIX, 1996)"})
			for name, data := range test.files {
				host.FS.(fstest.MapFS)[name] = &fstest.MapFile{Data: []byte(data)}
			}
			host.Metadata = testMetadata(server.URL)
			info := collectCloud(t, host)
			provider := ""
			if info.Provider != nil {
				provider = *info.Provider
			}
			if provider != test.provider || (provider != "" && (*info.Zone != "rtm0" || *info.Source != CloudSourceMetadata)) {
				t.Fatalf("info = %s", cloudJSON(t, info))
			}
			asked := false
			for _, path := range server.paths() {
				asked = asked || path == "GET /cloudinit/meta-data"
			}
			if asked != test.asked {
				t.Fatalf("asked the TransIP seed = %v, requested %v", asked, server.paths())
			}
		})
	}
}

func TestMetadataClientIgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	transport := NewMetadata().Client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("metadata client uses a proxy")
	}
	server := newMetadataServer(t, metadataSpec{provider: "hetzner"})
	host := dmiHost(map[string]string{"sys_vendor": "Hetzner"})
	host.Metadata = testMetadata(server.URL)
	if info := collectCloud(t, host); info.Source == nil || *info.Source != CloudSourceMetadata {
		t.Fatalf("info = %s", cloudJSON(t, info))
	}
}

func TestYAMLScalars(t *testing.T) {
	values := yamlScalars("a: 1\nb: 'two'\nlist:\n- x\nnested:\n  c: 3\nblock: |\n  text\nd: \"four\"\n")
	if len(values) != 3 || values["a"] != "1" || values["b"] != "two" || values["d"] != "four" {
		t.Fatalf("values = %v", values)
	}
}
