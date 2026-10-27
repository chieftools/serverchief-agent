package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Cloud providers.
const (
	CloudHetzner      = "hetzner"
	CloudAWS          = "aws"
	CloudDigitalOcean = "digitalocean"
	CloudGCP          = "gcp"
	CloudAzure        = "azure"
	CloudOVH          = "ovh"
	CloudOracle       = "oracle"
	CloudVultr        = "vultr"
	CloudLinode       = "linode"
	CloudScaleway     = "scaleway"
	CloudOpenStack    = "openstack"
	CloudTransIP      = "transip"
)

// Cloud sources.
const (
	CloudSourceMetadata = "metadata"
	CloudSourceDMI      = "dmi"
)

// CloudInfo is the payload of the cloud dataset. Every field is null when
// nothing is detected.
type CloudInfo struct {
	Provider     *string `json:"provider"`
	Source       *string `json:"source"`
	Region       *string `json:"region"`
	Zone         *string `json:"zone"`
	InstanceType *string `json:"instance_type"`
	InstanceID   *string `json:"instance_id"`
	PublicIPv4   *string `json:"public_ipv4"`
	PublicIPv6   *string `json:"public_ipv6"`
}

// cloudFields are the values a metadata service returned.
type cloudFields struct {
	provider     string
	region       string
	zone         string
	instanceType string
	instanceID   string
	publicIPv4   string
	publicIPv6   string
}

// Metadata reaches cloud metadata services. It only ever requests the
// leaf values the cloud dataset stores: no user data, vendor data,
// credentials or full instance documents that embed them.
type Metadata struct {
	// Client is used for every service. It must not use a proxy.
	Client *http.Client
	// ScalewayClient sends from a privileged source port, which Scaleway's
	// /conf requires.
	ScalewayClient *http.Client
	// LinkLocalURL is the base of the 169.254.169.254 services (AWS, Azure,
	// DigitalOcean, Hetzner, Linode, OpenStack, Oracle, Vultr).
	LinkLocalURL string
	GoogleURL    string
	ScalewayURL  string
	// TransIPURL is the base of the NoCloud seed TransIP names in DMI and
	// the cloud-init datasource; it answers each VPS by its source address.
	TransIPURL string
	// ProbeTimeout bounds the whole probe when DMI names no provider.
	ProbeTimeout time.Duration
	// Timeout bounds the queries to a provider that DMI identified.
	Timeout time.Duration
}

const (
	metadataConnectTimeout = 500 * time.Millisecond
	metadataProbeTimeout   = 2 * time.Second
	metadataTimeout        = 5 * time.Second
	metadataMaxBody        = 64 << 10
)

// NewMetadata returns the client for the real metadata services. Proxies
// from the environment are never used: the services are link-local, or
// like TransIP's only answer the host's own address.
func NewMetadata() *Metadata {
	return &Metadata{
		Client:         metadataHTTPClient(nil),
		ScalewayClient: metadataHTTPClient(privilegedPortControl),
		LinkLocalURL:   "http://169.254.169.254",
		GoogleURL:      "http://metadata.google.internal",
		ScalewayURL:    "http://169.254.42.42",
		TransIPURL:     "http://" + transIPSeedHost + "/cloudinit",
		ProbeTimeout:   metadataProbeTimeout,
		Timeout:        metadataTimeout,
	}
}

func metadataHTTPClient(localAddr func() *net.TCPAddr) *http.Client {
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialer := &net.Dialer{Timeout: metadataConnectTimeout}
		if localAddr != nil {
			// Try a few privileged ports; one may be taken. Without root
			// the bind fails and the plain dial below is used.
			for range 8 {
				dialer.LocalAddr = localAddr()
				if connection, err := dialer.DialContext(ctx, network, address); err == nil {
					return connection, nil
				} else if !errors.Is(err, syscall.EADDRINUSE) && !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) {
					return nil, err
				}
			}
			dialer.LocalAddr = nil
		}
		return dialer.DialContext(ctx, network, address)
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dial,
			ResponseHeaderTimeout: metadataProbeTimeout,
			MaxIdleConns:          2,
			IdleConnTimeout:       5 * time.Second,
			DisableCompression:    true,
		},
		// Metadata services do not redirect; never follow one off-link.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

var privilegedPortCounter atomic.Uint32

// privilegedPortControl cycles through the source ports 1023 down to 512.
func privilegedPortControl() *net.TCPAddr {
	return &net.TCPAddr{Port: 1023 - int(privilegedPortCounter.Add(1)%512)}
}

// Cloud collects the cloud dataset.
type Cloud struct {
	host *Host
}

func NewCloud(host *Host) *Cloud { return &Cloud{host: host} }

func (*Cloud) Name() string { return "cloud" }

func (collector *Cloud) Collect(ctx context.Context) (any, error) {
	host := collector.host
	provider := host.cloudFromDMI()
	metadata := host.Metadata
	info := &CloudInfo{}
	if metadata == nil {
		info.setDMI(provider)
		return info, nil
	}
	defer metadata.Client.CloseIdleConnections()
	if metadata.ScalewayClient != nil {
		defer metadata.ScalewayClient.CloseIdleConnections()
	}

	if provider == "" {
		fields, ok := metadata.probe(ctx)
		if ok {
			info.setMetadata(fields)
		}
		return info, nil
	}

	timeout := metadata.Timeout
	if timeout <= 0 {
		timeout = metadataTimeout
	}
	queryContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fields, err := metadata.query(queryContext, provider)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		info.setDMI(provider)
		return info, nil
	}
	info.setMetadata(fields)
	return info, nil
}

func (info *CloudInfo) setDMI(provider string) {
	if provider == "" {
		return
	}
	info.Provider = optionalString(provider)
	info.Source = optionalString(CloudSourceDMI)
}

func (info *CloudInfo) setMetadata(fields cloudFields) {
	info.Provider = optionalString(fields.provider)
	info.Source = optionalString(CloudSourceMetadata)
	info.Region = optionalString(strings.TrimSpace(fields.region))
	info.Zone = optionalString(strings.TrimSpace(fields.zone))
	info.InstanceType = optionalString(strings.TrimSpace(fields.instanceType))
	info.InstanceID = optionalString(strings.TrimSpace(fields.instanceID))
	info.PublicIPv4 = publicAddress(fields.publicIPv4, true)
	info.PublicIPv6 = publicAddress(fields.publicIPv6, false)
}

// publicAddress validates an address (optionally with a prefix length, as
// Linode reports it) and returns it when it has the right family and is
// public.
func publicAddress(value string, ipv4 bool) *string {
	value = strings.TrimSpace(value)
	if prefix, err := netip.ParsePrefix(value); err == nil {
		value = prefix.Addr().String()
	}
	address, err := netip.ParseAddr(value)
	if err != nil || address.Unmap().Is4() != ipv4 || ClassifyAddress(address) != ScopePublic {
		return nil
	}
	text := address.Unmap().String()
	return &text
}

// transIPSeedHost serves TransIP's NoCloud seed. Only this host is ever
// requested: the seed URL in DMI or the datasource identifies TransIP, it is
// not followed.
const transIPSeedHost = "pxe.frontend.transip.nl"

// Azure publishes this asset tag on every VM; Hyper-V elsewhere has the
// same vendor and product names.
const azureAssetTag = "7783-7084-3265-9085-8269-3286-77"

// cloudFromDMI names the provider from DMI, or returns "" when DMI is
// inconclusive.
func (host *Host) cloudFromDMI() string {
	read := func(name string) string {
		return strings.ToLower(host.readOptional("/sys/class/dmi/id/" + name))
	}
	vendor, product, bios, asset, board := read("sys_vendor"), read("product_name"), read("bios_vendor"), read("chassis_asset_tag"), read("board_vendor")
	all := strings.Join([]string{vendor, product, bios, asset, board}, "\n")
	switch {
	case asset == azureAssetTag:
		return CloudAzure
	case strings.Contains(asset, "oraclecloud"):
		return CloudOracle
	case strings.Contains(all, "amazon ec2") || strings.HasPrefix(strings.ToLower(host.readOptional("/sys/hypervisor/uuid")), "ec2"):
		return CloudAWS
	case strings.Contains(vendor, "google") || strings.Contains(product, "google compute engine"):
		return CloudGCP
	case strings.Contains(all, "digitalocean"):
		return CloudDigitalOcean
	case strings.Contains(vendor, "hetzner") || strings.Contains(bios, "hetzner"):
		return CloudHetzner
	case strings.Contains(all, "vultr"):
		return CloudVultr
	case strings.Contains(all, "linode") || strings.Contains(vendor, "akamai"):
		return CloudLinode
	case strings.Contains(all, "scaleway"):
		return CloudScaleway
	case strings.Contains(all, "ovh"):
		return CloudOVH
	case strings.Contains(all, "openstack"):
		return CloudOpenStack
	case host.transIPSeeded(read("product_serial")):
		return CloudTransIP
	}
	return ""
}

// transIPSeeded tells whether cloud-init seeds this host from TransIP. The
// DMI serial names the seed but only root may read it, so the datasource
// cloud-init recorded on first boot, which stays readable after cloud-init
// is disabled, names it for the unprivileged agent.
func (host *Host) transIPSeeded(serial string) bool {
	seed := "s=http://" + transIPSeedHost + "/"
	return strings.Contains(serial, seed) || strings.Contains(strings.ToLower(host.readOptional("/var/lib/cloud/instance/datasource")), "seed=dmi,http://"+transIPSeedHost+"/")
}

// query reads the metadata of a provider that DMI identified.
func (metadata *Metadata) query(ctx context.Context, provider string) (cloudFields, error) {
	switch provider {
	case CloudHetzner:
		return metadata.hetzner(ctx)
	case CloudAWS:
		return metadata.aws(ctx)
	case CloudDigitalOcean:
		return metadata.digitalOcean(ctx)
	case CloudGCP:
		return metadata.gcp(ctx)
	case CloudAzure:
		return metadata.azure(ctx)
	case CloudOVH, CloudOpenStack:
		fields, err := metadata.openStack(ctx)
		if err == nil && provider == CloudOVH {
			fields.provider = CloudOVH
		}
		return fields, err
	case CloudOracle:
		return metadata.oracle(ctx)
	case CloudVultr:
		return metadata.vultr(ctx)
	case CloudLinode:
		return metadata.linode(ctx)
	case CloudScaleway:
		return metadata.scaleway(ctx)
	case CloudTransIP:
		return metadata.transIP(ctx)
	}
	return cloudFields{}, fmt.Errorf("unknown provider %q", provider)
}

// probe asks the link-local address once, within ProbeTimeout, whose
// metadata service it is. The first request also tells whether anything
// answers at all: a connection error ends the probe at once, so hosts
// without metadata return after at most the 500 ms connect timeout.
func (metadata *Metadata) probe(ctx context.Context) (cloudFields, bool) {
	timeout := metadata.ProbeTimeout
	if timeout <= 0 {
		timeout = metadataProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	probes := []func(context.Context) (cloudFields, error){
		metadata.hetzner,
		metadata.aws,
		metadata.digitalOcean,
		metadata.linode,
		metadata.openStack,
	}
	for _, probe := range probes {
		fields, err := probe(ctx)
		if err == nil {
			return fields, true
		}
		var status *metadataStatusError
		if !errors.As(err, &status) {
			// Nothing answered, or the budget is spent.
			return cloudFields{}, false
		}
	}
	return cloudFields{}, false
}

// metadataStatusError is an HTTP answer other than 200: something is
// listening, but it is not this provider's service (or not this path).
type metadataStatusError struct {
	url    string
	status int
}

func (err *metadataStatusError) Error() string {
	return fmt.Sprintf("%s: HTTP %d", err.url, err.status)
}

func (metadata *Metadata) fetch(ctx context.Context, client *http.Client, method, url string, headers map[string]string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return "", err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	if client == nil {
		client = metadata.Client
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, metadataMaxBody))
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", &metadataStatusError{url: url, status: response.StatusCode}
	}
	return strings.TrimSpace(string(body)), nil
}

func (metadata *Metadata) get(ctx context.Context, url string, headers map[string]string) (string, error) {
	return metadata.fetch(ctx, nil, http.MethodGet, url, headers)
}

// optional returns "" for a missing (404) leaf and the error otherwise.
func optional(value string, err error) (string, error) {
	var status *metadataStatusError
	if errors.As(err, &status) && status.status == http.StatusNotFound {
		return "", nil
	}
	return value, err
}

// hetzner reads https://docs.hetzner.cloud/#server-metadata: one YAML
// document of which only top-level scalars are read. Hetzner's "region" is
// the network zone (eu-central); the location (fsn1) is the availability
// zone without its "-dc14" suffix.
func (metadata *Metadata) hetzner(ctx context.Context) (cloudFields, error) {
	document, err := metadata.get(ctx, metadata.LinkLocalURL+"/hetzner/v1/metadata", nil)
	if err != nil {
		return cloudFields{}, err
	}
	values := yamlScalars(document)
	if values["instance-id"] == "" && values["availability-zone"] == "" {
		return cloudFields{}, &metadataStatusError{url: "/hetzner/v1/metadata", status: http.StatusNoContent}
	}
	fields := cloudFields{
		provider:   CloudHetzner,
		zone:       values["availability-zone"],
		region:     values["region"],
		instanceID: values["instance-id"],
		publicIPv4: values["public-ipv4"],
	}
	if location, _, ok := strings.Cut(fields.zone, "-dc"); ok && location != "" {
		fields.region = location
	}
	return fields, nil
}

// transIP reads the NoCloud meta-data TransIP serves each VPS: top-level
// scalars of which "region" is the country (nl) and the availability zone
// the data centre (ams0, rtm0), which is reported as the region too.
func (metadata *Metadata) transIP(ctx context.Context) (cloudFields, error) {
	document, err := metadata.get(ctx, metadata.TransIPURL+"/meta-data", nil)
	if err != nil {
		return cloudFields{}, err
	}
	values := yamlScalars(document)
	zone := strings.ToLower(values["availability_zone"])
	if values["instance-id"] == "" || zone == "" {
		// Any other address gets a document without the instance.
		return cloudFields{}, &metadataStatusError{url: "/cloudinit/meta-data", status: http.StatusNoContent}
	}
	return cloudFields{
		provider:   CloudTransIP,
		region:     zone,
		zone:       zone,
		instanceID: values["instance-id"],
	}, nil
}

// yamlScalars returns the unindented "key: value" lines of a YAML
// document. Nested blocks, lists and block scalars are ignored.
func yamlScalars(document string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(document, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' || line[0] == '-' {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" || value == "|" || value == ">" || strings.HasPrefix(value, "|") || strings.HasPrefix(value, ">") || strings.HasPrefix(value, "[") || strings.HasPrefix(value, "{") {
			continue
		}
		values[strings.TrimSpace(key)] = unquote(value)
	}
	return values
}

// aws uses IMDSv2: a session token from PUT /latest/api/token, then the
// leaf values. https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/instancedata-data-retrieval.html
func (metadata *Metadata) aws(ctx context.Context) (cloudFields, error) {
	token, err := metadata.fetch(ctx, nil, http.MethodPut, metadata.LinkLocalURL+"/latest/api/token", map[string]string{"X-aws-ec2-metadata-token-ttl-seconds": "60"})
	if err != nil {
		return cloudFields{}, err
	}
	headers := map[string]string{"X-aws-ec2-metadata-token": token}
	base := metadata.LinkLocalURL + "/latest/meta-data/"
	fields := cloudFields{provider: CloudAWS}
	for _, leaf := range []struct {
		path   string
		target *string
	}{
		{"placement/region", &fields.region},
		{"placement/availability-zone", &fields.zone},
		{"instance-type", &fields.instanceType},
		{"instance-id", &fields.instanceID},
		{"public-ipv4", &fields.publicIPv4},
		{"ipv6", &fields.publicIPv6},
	} {
		if *leaf.target, err = optional(metadata.get(ctx, base+leaf.path, headers)); err != nil {
			return cloudFields{}, err
		}
	}
	if fields.instanceID == "" {
		return cloudFields{}, errors.New("aws: no instance-id")
	}
	return fields, nil
}

// digitalOcean reads leaf paths of https://docs.digitalocean.com/reference/api/metadata/
// rather than /metadata/v1.json, which embeds the droplet's user data.
func (metadata *Metadata) digitalOcean(ctx context.Context) (cloudFields, error) {
	base := metadata.LinkLocalURL + "/metadata/v1/"
	id, err := metadata.get(ctx, base+"id", nil)
	if err != nil {
		return cloudFields{}, err
	}
	fields := cloudFields{provider: CloudDigitalOcean, instanceID: id}
	for _, leaf := range []struct {
		path   string
		target *string
	}{
		{"region", &fields.region},
		{"interfaces/public/0/ipv4/address", &fields.publicIPv4},
		{"interfaces/public/0/ipv6/address", &fields.publicIPv6},
	} {
		if *leaf.target, err = optional(metadata.get(ctx, base+leaf.path, nil)); err != nil {
			return cloudFields{}, err
		}
	}
	return fields, nil
}

// gcp reads https://cloud.google.com/compute/docs/metadata/predefined-metadata-keys.
// The zone comes as projects/<n>/zones/europe-west4-a; the region is the
// zone without its last part.
func (metadata *Metadata) gcp(ctx context.Context) (cloudFields, error) {
	headers := map[string]string{"Metadata-Flavor": "Google"}
	base := metadata.GoogleURL + "/computeMetadata/v1/instance/"
	zone, err := metadata.get(ctx, base+"zone", headers)
	if err != nil {
		return cloudFields{}, err
	}
	fields := cloudFields{provider: CloudGCP, zone: lastPathPart(zone)}
	if index := strings.LastIndexByte(fields.zone, '-'); index > 0 {
		fields.region = fields.zone[:index]
	}
	for _, leaf := range []struct {
		path   string
		target *string
	}{
		{"machine-type", &fields.instanceType},
		{"id", &fields.instanceID},
		{"network-interfaces/0/access-configs/0/external-ip", &fields.publicIPv4},
	} {
		if *leaf.target, err = optional(metadata.get(ctx, base+leaf.path, headers)); err != nil {
			return cloudFields{}, err
		}
	}
	fields.instanceType = lastPathPart(fields.instanceType)
	return fields, nil
}

func lastPathPart(value string) string {
	return value[strings.LastIndexByte(value, '/')+1:]
}

// azure reads text leaves of https://learn.microsoft.com/azure/virtual-machines/instance-metadata-service.
func (metadata *Metadata) azure(ctx context.Context) (cloudFields, error) {
	headers := map[string]string{"Metadata": "true"}
	leafURL := func(leaf string) string {
		return metadata.LinkLocalURL + "/metadata/instance/" + leaf + "?api-version=2021-02-01&format=text"
	}
	fields := cloudFields{provider: CloudAzure}
	var err error
	for _, leaf := range []struct {
		path   string
		target *string
	}{
		{"compute/location", &fields.region},
		{"compute/zone", &fields.zone},
		{"compute/vmSize", &fields.instanceType},
		{"compute/vmId", &fields.instanceID},
		{"network/interface/0/ipv4/ipAddress/0/publicIpAddress", &fields.publicIPv4},
	} {
		if *leaf.target, err = optional(metadata.get(ctx, leafURL(leaf.path), headers)); err != nil {
			return cloudFields{}, err
		}
	}
	if fields.region == "" {
		return cloudFields{}, errors.New("azure: no location")
	}
	return fields, nil
}

// openStack reads the EC2-compatible leaves that Nova's metadata service
// serves next to /openstack/latest/meta_data.json. meta_data.json itself is
// not read because it can embed admin_pass. OVH is told apart by its
// hostnames (*.ovh.net, *.ovh.us, *.ovh.ca); Nova's default "nova" zone
// carries no location and is dropped.
func (metadata *Metadata) openStack(ctx context.Context) (cloudFields, error) {
	if _, err := metadata.get(ctx, metadata.LinkLocalURL+"/openstack", nil); err != nil {
		return cloudFields{}, err
	}
	base := metadata.LinkLocalURL + "/latest/meta-data/"
	fields := cloudFields{provider: CloudOpenStack}
	var hostname string
	var err error
	for _, leaf := range []struct {
		path   string
		target *string
	}{
		{"placement/availability-zone", &fields.zone},
		{"instance-type", &fields.instanceType},
		{"instance-id", &fields.instanceID},
		{"public-ipv4", &fields.publicIPv4},
		{"hostname", &hostname},
	} {
		if *leaf.target, err = optional(metadata.get(ctx, base+leaf.path, nil)); err != nil {
			return cloudFields{}, err
		}
	}
	if strings.EqualFold(fields.zone, "nova") {
		fields.zone = ""
	}
	hostname = strings.ToLower(hostname)
	for _, suffix := range []string{".ovh.net", ".ovh.us", ".ovh.ca", ".ovhcloud.com"} {
		if strings.HasSuffix(hostname, suffix) {
			fields.provider = CloudOVH
		}
	}
	return fields, nil
}

// oracle reads leaves of https://docs.oracle.com/iaas/Content/Compute/Tasks/gettingmetadata.htm;
// the full /opc/v2/instance/ document embeds user_data.
func (metadata *Metadata) oracle(ctx context.Context) (cloudFields, error) {
	headers := map[string]string{"Authorization": "Bearer Oracle"}
	base := metadata.LinkLocalURL + "/opc/v2/instance/"
	fields := cloudFields{provider: CloudOracle}
	var err error
	for _, leaf := range []struct {
		path   string
		target *string
	}{
		{"canonicalRegionName", &fields.region},
		{"availabilityDomain", &fields.zone},
		{"shape", &fields.instanceType},
		{"id", &fields.instanceID},
	} {
		if *leaf.target, err = optional(metadata.get(ctx, base+leaf.path, headers)); err != nil {
			return cloudFields{}, err
		}
	}
	if fields.instanceID == "" {
		return cloudFields{}, errors.New("oracle: no instance id")
	}
	return fields, nil
}

// vultr reads https://www.vultr.com/metadata/ (/v1.json), which holds no
// user data.
func (metadata *Metadata) vultr(ctx context.Context) (cloudFields, error) {
	document, err := metadata.get(ctx, metadata.LinkLocalURL+"/v1.json", nil)
	if err != nil {
		return cloudFields{}, err
	}
	var parsed struct {
		InstanceID   string `json:"instanceid"`
		InstanceV2ID string `json:"instance-v2-id"`
		Region       struct {
			RegionCode string `json:"regioncode"`
		} `json:"region"`
		Interfaces []struct {
			NetworkType string `json:"network-type"`
			IPv4        struct {
				Address string `json:"address"`
			} `json:"ipv4"`
			IPv6 struct {
				Address string `json:"address"`
			} `json:"ipv6"`
		} `json:"interfaces"`
	}
	if err := json.Unmarshal([]byte(document), &parsed); err != nil {
		return cloudFields{}, fmt.Errorf("vultr: %w", err)
	}
	fields := cloudFields{provider: CloudVultr, region: strings.ToLower(parsed.Region.RegionCode), instanceID: parsed.InstanceV2ID}
	if fields.instanceID == "" {
		fields.instanceID = parsed.InstanceID
	}
	for _, iface := range parsed.Interfaces {
		if iface.NetworkType == "public" || iface.NetworkType == "" {
			fields.publicIPv4, fields.publicIPv6 = iface.IPv4.Address, iface.IPv6.Address
			break
		}
	}
	return fields, nil
}

// linode uses https://techdocs.akamai.com/cloud-computing/docs/overview-of-the-metadata-service:
// a token from PUT /v1/token, then /v1/instance and /v1/network as JSON.
func (metadata *Metadata) linode(ctx context.Context) (cloudFields, error) {
	token, err := metadata.fetch(ctx, nil, http.MethodPut, metadata.LinkLocalURL+"/v1/token", map[string]string{"Metadata-Token-Expiry-Seconds": "60"})
	if err != nil {
		return cloudFields{}, err
	}
	headers := map[string]string{"Metadata-Token": token, "Accept": "application/json"}
	document, err := metadata.get(ctx, metadata.LinkLocalURL+"/v1/instance", headers)
	if err != nil {
		return cloudFields{}, err
	}
	var instance struct {
		ID     json.Number `json:"id"`
		Region string      `json:"region"`
		Type   string      `json:"type"`
	}
	if err := json.Unmarshal([]byte(document), &instance); err != nil {
		return cloudFields{}, fmt.Errorf("linode: %w", err)
	}
	fields := cloudFields{provider: CloudLinode, region: instance.Region, instanceType: instance.Type, instanceID: instance.ID.String()}
	if document, err := metadata.get(ctx, metadata.LinkLocalURL+"/v1/network", headers); err == nil {
		var network struct {
			IPv4 struct {
				Public []string `json:"public"`
			} `json:"ipv4"`
			IPv6 struct {
				SLAAC string `json:"slaac"`
			} `json:"ipv6"`
		}
		if json.Unmarshal([]byte(document), &network) == nil {
			if len(network.IPv4.Public) > 0 {
				fields.publicIPv4 = network.IPv4.Public[0]
			}
			fields.publicIPv6 = network.IPv6.SLAAC
		}
	}
	return fields, nil
}

// scaleway reads https://www.scaleway.com/en/docs/instances/ (169.254.42.42/conf),
// which needs a privileged source port and holds no user data.
func (metadata *Metadata) scaleway(ctx context.Context) (cloudFields, error) {
	document, err := metadata.fetch(ctx, metadata.ScalewayClient, http.MethodGet, metadata.ScalewayURL+"/conf?format=json", nil)
	if err != nil {
		return cloudFields{}, err
	}
	var parsed struct {
		ID             string `json:"id"`
		CommercialType string `json:"commercial_type"`
		Zone           string `json:"zone"`
		Location       struct {
			ZoneID string `json:"zone_id"`
		} `json:"location"`
		PublicIP struct {
			Address string `json:"address"`
		} `json:"public_ip"`
		IPv6 *struct {
			Address string `json:"address"`
		} `json:"ipv6"`
	}
	if err := json.Unmarshal([]byte(document), &parsed); err != nil {
		return cloudFields{}, fmt.Errorf("scaleway: %w", err)
	}
	fields := cloudFields{provider: CloudScaleway, zone: parsed.Location.ZoneID, instanceType: parsed.CommercialType, instanceID: parsed.ID, publicIPv4: parsed.PublicIP.Address}
	if fields.zone == "" {
		fields.zone = parsed.Zone
	}
	if index := strings.LastIndexByte(fields.zone, '-'); index > 0 {
		fields.region = fields.zone[:index]
	}
	if parsed.IPv6 != nil {
		fields.publicIPv6 = parsed.IPv6.Address
	}
	return fields, nil
}
