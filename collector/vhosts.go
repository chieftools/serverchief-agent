package collector

import (
	"cmp"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Virtual host sources, in the order they are collected.
const (
	VHostSourceApache      = "apache"
	VHostSourceNginx       = "nginx"
	VHostSourcePlesk       = "plesk"
	VHostSourceDirectAdmin = "directadmin"
)

// PHP handlers.
const (
	PHPHandlerFPM     = "fpm"
	PHPHandlerFastCGI = "fastcgi"
	PHPHandlerCGI     = "cgi"
	PHPHandlerModule  = "module"
	PHPHandlerLSAPI   = "lsapi"
)

// VHostsInfo is the payload of the vhosts dataset, sent as a delta keyed by
// source/name.
type VHostsInfo struct {
	Items []VHost `json:"items"`
}

// VHost is one site of one source. Every name has one item per source; its
// ports cover every block that serves the name.
type VHost struct {
	Source string `json:"source"`
	// Name is the primary server name in lower case.
	Name string `json:"name"`
	// Aliases are sorted and deduplicated; wildcards are kept as written.
	Aliases []string `json:"aliases"`
	Ports   []int    `json:"ports"`
	// SSL is true when any of the ports serves TLS.
	SSL bool `json:"ssl"`
	// HTTP2 is true when a block of the site offers HTTP/2, HTTP3 when
	// one listens for QUIC and offers HTTP/3.
	HTTP2        bool              `json:"http2"`
	HTTP3        bool              `json:"http3"`
	DocumentRoot *string           `json:"document_root"`
	PHP          *VHostPHP         `json:"php"`
	Certificate  *VHostCertificate `json:"certificate"`
	// ConfigFile is the file that defines the site (without a line number,
	// so editing other sites in the same file does not change the item).
	ConfigFile *string `json:"config_file"`
	// Owner is the Plesk subscription or DirectAdmin user, or on a plain
	// web server the user whose home directory holds the document root;
	// null otherwise.
	Owner *string `json:"owner"`
	// Redirect is set when the site only redirects elsewhere: a redirect
	// to a site of the same source is folded into that site instead.
	Redirect *VHostRedirect `json:"redirect"`
	// HTTPSRedirect is true when plain HTTP requests for the site are
	// redirected to HTTPS on the same name.
	HTTPSRedirect bool `json:"https_redirect"`

	// subdomain marks a panel subdomain, whose www alias the panel adds
	// on its own; it is not sent.
	subdomain bool
}

// VHostRedirect is where a site that only redirects sends its requests.
type VHostRedirect struct {
	// Status is the HTTP status: 301, 302, 303, 307 or 308.
	Status int `json:"status"`
	// Target is the URL as configured, variables included
	// (https://$host$request_uri, https://%{SERVER_NAME}%{REQUEST_URI}).
	Target string `json:"target"`
	// Host is the target's host name in lower case, or null when the
	// target keeps the requested host.
	Host *string `json:"host"`

	// https is true when the target's scheme is https; it is not sent.
	https bool
}

// VHostPHP describes how a site runs PHP.
type VHostPHP struct {
	// Version is major.minor, or null when it cannot be determined.
	Version *string `json:"version"`
	// Handler is fpm, fastcgi, cgi, module or lsapi; null when unknown.
	Handler *string `json:"handler"`
	// weak marks a guess from php_value-style directives alone, which
	// mod_php and mod_lsapi both accept; it is not sent.
	weak bool
}

// VHostCertificate holds the public facts of a site's first certificate.
type VHostCertificate struct {
	Subject  string   `json:"subject"`
	Issuer   string   `json:"issuer"`
	NotAfter string   `json:"not_after"`
	Names    []string `json:"names"`
	// Fingerprint is the SHA-256 of the leaf's DER, lowercase hex: the
	// key of the certificate in the certificates dataset.
	Fingerprint string `json:"fingerprint"`

	// source tells the certificates dataset where the certificate is
	// configured; it is not sent.
	source *certificateSource
}

// certificateSource is where a site's certificate is configured: a file
// with an optional chain file, or inline PEM (Plesk's database).
type certificateSource struct {
	path  string
	chain string
	// inline holds certificates that do not come from a file, leaf first;
	// inlineChain their intermediates.
	inline      []*x509.Certificate
	inlineChain []*x509.Certificate
	// managed is what the panel says about the certificate's renewal.
	managed *CertificateManager
}

// VHostKey is the delta key: source/name.
func VHostKey(item VHost) string {
	return item.Source + "/" + item.Name
}

// Normalize sorts items by their delta key, bytewise, and the lists inside
// each item.
func (info *VHostsInfo) Normalize() {
	if info.Items == nil {
		info.Items = []VHost{}
	}
	for index := range info.Items {
		item := &info.Items[index]
		item.Aliases = sortedUnique(item.Aliases)
		item.Ports = sortedUnique(item.Ports)
		if item.Certificate != nil {
			item.Certificate.Names = sortedUnique(item.Certificate.Names)
		}
	}
	slices.SortFunc(info.Items, func(a, b VHost) int {
		return strings.Compare(VHostKey(a), VHostKey(b))
	})
}

// sortedUnique returns a sorted, deduplicated copy of values, never nil.
// It copies because items are shared with the previous-result cache.
func sortedUnique[T cmp.Ordered](values []T) []T {
	result := slices.Clone(values)
	slices.Sort(result)
	result = slices.Compact(result)
	if result == nil {
		result = []T{}
	}
	return result
}

// vhostSource is one sub-collector. present tells whether its web server
// or panel is installed; only present sources run.
type vhostSource struct {
	name    string
	present func(host *Host) bool
	collect func(ctx context.Context, run *vhostRun) ([]VHost, error)
}

var vhostSources = []vhostSource{
	{VHostSourceApache, func(host *Host) bool { return host.apacheControl() != "" }, collectApache},
	{VHostSourceNginx, func(host *Host) bool { return host.findCommand("nginx") != "" }, collectNginx},
	{VHostSourcePlesk, func(host *Host) bool { return host.isDir(pleskRoot) && host.findCommand("plesk") != "" }, collectPlesk},
	{VHostSourceDirectAdmin, func(host *Host) bool { return host.isDir(directAdminUsers) }, collectDirectAdmin},
}

// vhostChangeRoots are the configuration trees whose modification times
// trigger an early collection. Plesk rewrites the per-domain web server
// configuration under /var/www/vhosts/system whenever a domain changes;
// only its conf directories count (see ignoredChange).
var vhostChangeRoots = []string{
	"/etc/apache2",
	"/etc/httpd",
	"/etc/nginx",
	"/etc/letsencrypt/live",
	"/var/www/vhosts/system",
	"/usr/local/directadmin/data/users",
}

const (
	// vhostChangeDepth reaches DirectAdmin's users/<user>/domains/<domain>.conf
	// and Plesk's system/<domain>/conf/httpd.conf.
	vhostChangeDepth = 3
	// vhostChangeLimit bounds the entries stat'ed per root and poll.
	vhostChangeLimit = 20000
)

// pleskStatisticsFiles are the files Plesk's daily statistics run writes
// to every domain's conf directory. They hold no web server configuration.
var pleskStatisticsFiles = []string{"stat_ttl.conf", "prev_month.found"}

// vhostSnapshotMaxAge bounds how old a vhosts collection may be for the
// certificates and applications datasets to reuse it instead of collecting
// again, as long as the configuration has not changed since.
const vhostSnapshotMaxAge = 30 * time.Minute

// vhostSnapshotFresh is how old a vhosts collection may be to be reused
// even when the configuration changed since it. One inventory round
// collects applications, certificates and vhosts one after another, so the
// first of them collects and the others reuse its result instead of
// running apachectl and plesk db again.
const vhostSnapshotFresh = time.Minute

// vhostSnapshot is the last successful collection, with the change token
// taken before it.
type vhostSnapshot struct {
	token string
	at    time.Time
	items []VHost
}

// VHosts collects the vhosts dataset from Apache, nginx, Plesk and
// DirectAdmin.
//
// A source that fails is logged and left out, and the dataset fails only
// when every present source failed. A source that failed after an earlier
// success keeps its previous items, so a briefly broken configuration (an
// unreadable file during an edit) does not remove its sites from the
// backend and add them back on the next run. A source that is not ready
// yet (ErrNotReady) without earlier items fails the whole dataset, so its
// sites are not reported as gone while it starts.
type VHosts struct {
	host *Host

	mu   sync.Mutex
	last map[string][]VHost
	// plesk tracks how long the Plesk database has been failing.
	plesk pleskState
	// warned holds each source's last logged warnings, so a lasting
	// condition (a missing include) is logged once, not every run.
	warned map[string]string
	// store holds the parsed certificate files, shared with the
	// certificates dataset.
	store    *certificateStore
	snapshot *vhostSnapshot
}

func NewVHosts(host *Host) *VHosts {
	return &VHosts{host: host, last: make(map[string][]VHost), warned: make(map[string]string), store: newCertificateStore()}
}

func (*VHosts) Name() string { return "vhosts" }

// DeltaKeyFields implements KeyedDataset.
func (*VHosts) DeltaKeyFields() []string { return []string{"source", "name"} }

// ChangeToken implements ChangeDetector with the names, modification times
// and sizes of the entries in the web server and panel configuration trees,
// up to vhostChangeDepth levels deep.
func (collector *VHosts) ChangeToken() string {
	digest := fnv.New64a()
	treeDigest(digest, collector.host, vhostChangeRoots, vhostChangeDepth, vhostChangeLimit)
	return fmt.Sprintf("%016x", digest.Sum64())
}

// treeDigest writes the names, modification times and sizes of the entries
// under roots, up to depth levels deep and limit entries per root, leaving
// out the entries ignoredChange names.
func treeDigest(digest io.Writer, host *Host, roots []string, depth, limit int) {
	for _, root := range roots {
		count := 0
		_ = fs.WalkDir(host.FS, relative(root), func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if ignoredChange(name) {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if count++; count > limit {
				return fs.SkipAll
			}
			info, err := entry.Info()
			if err != nil {
				return nil
			}
			fmt.Fprintf(digest, "%s\x00%d\x00%d\n", name, info.ModTime().UnixNano(), info.Size())
			if entry.IsDir() && strings.Count(strings.TrimPrefix(name, relative(root)), "/") >= depth {
				return fs.SkipDir
			}
			return nil
		})
	}
}

// ignoredChange reports whether an entry of the change roots (a path
// relative to the host's root) is left out of the change tokens: the
// entries of Plesk's per-domain directories under /var/www/vhosts/system
// other than conf, and the statistics files in conf. The logs directory
// there holds the access and error logs, which the web servers write on
// every request, so a busy server would otherwise look like its
// configuration changed at every poll.
func ignoredChange(name string) bool {
	domainEntry, ok := strings.CutPrefix(name, relative(pleskVhostsSystem)+"/")
	if !ok {
		return false
	}
	parts := strings.Split(domainEntry, "/")
	switch {
	case len(parts) < 2:
		return false
	case parts[1] != "conf":
		return true
	default:
		return len(parts) == 3 && slices.Contains(pleskStatisticsFiles, parts[2])
	}
}

// recent returns the items of a collection that is at most
// vhostSnapshotFresh old, or at most vhostSnapshotMaxAge old with its
// configuration unchanged since, collecting again otherwise. The
// certificates and applications datasets use it to find the sites without
// running apachectl and plesk db again.
func (collector *VHosts) recent(ctx context.Context) ([]VHost, error) {
	token := collector.ChangeToken()
	collector.mu.Lock()
	snapshot := collector.snapshot
	collector.mu.Unlock()
	if snapshot != nil {
		age := collector.host.now().Sub(snapshot.at)
		if age < vhostSnapshotFresh || (snapshot.token == token && age < vhostSnapshotMaxAge) {
			return snapshot.items, nil
		}
	}
	payload, err := collector.Collect(ctx)
	if err != nil {
		if snapshot != nil && ctx.Err() == nil {
			return snapshot.items, err
		}
		return nil, err
	}
	return payload.(*VHostsInfo).Items, nil
}

// Collect collects the sites again, unless another dataset of the same
// inventory round collected them less than vhostSnapshotFresh ago and the
// configuration has not changed since.
func (collector *VHosts) Collect(ctx context.Context) (any, error) {
	token := collector.ChangeToken()
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if snapshot := collector.snapshot; snapshot != nil && snapshot.token == token && collector.host.now().Sub(snapshot.at) < vhostSnapshotFresh {
		return &VHostsInfo{Items: slices.Clone(snapshot.items)}, nil
	}

	run := &vhostRun{host: collector.host, store: collector.store, certificates: make(map[string]*VHostCertificate), warnings: make(map[string][]string), plesk: &collector.plesk}
	info := &VHostsInfo{Items: []VHost{}}
	var failures []string
	// notReady is the first failure of a source that is still starting.
	var notReady error
	present := 0
	for _, source := range vhostSources {
		if !source.present(collector.host) {
			delete(collector.last, source.name)
			continue
		}
		present++
		items, err := source.collect(ctx, run)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			previous, ok := collector.last[source.name]
			if errors.Is(err, ErrNotReady) {
				// Without earlier items its sites would be reported as gone.
				if !ok {
					return nil, fmt.Errorf("%s: %w", source.name, err)
				}
				if notReady == nil {
					notReady = fmt.Errorf("%s: %w", source.name, err)
				}
			}
			failures = append(failures, source.name+": "+err.Error())
			if ok {
				log.Printf("[collector] vhosts: %s failed, keeping its %d previous items: %v", source.name, len(previous), err)
				info.Items = append(info.Items, previous...)
			} else {
				log.Printf("[collector] vhosts: %s failed: %v", source.name, err)
			}
			continue
		}
		if warnings := strings.Join(run.warnings[source.name], "; "); warnings != collector.warned[source.name] {
			if warnings != "" {
				log.Printf("[collector] vhosts: %s: %s", source.name, warnings)
			}
			collector.warned[source.name] = warnings
		}
		items = mergeVHosts(source.name, items)
		collector.last[source.name] = items
		info.Items = append(info.Items, items...)
	}
	if present > 0 && len(failures) == present {
		if notReady != nil {
			return nil, notReady
		}
		return nil, errors.New(strings.Join(failures, "; "))
	}
	refineVHosts(collector.host, info.Items)
	info.Normalize()
	collector.snapshot = &vhostSnapshot{token: token, at: collector.host.now(), items: slices.Clone(info.Items)}
	return info, nil
}

// refineVHosts applies what one source knows to the items of every
// source: the aliases Plesk and DirectAdmin add to their sites on their own
// are dropped (see implicitAlias) unless the item's certificate covers
// them, and a site no panel manages gets the user whose home directory
// holds its document root as its owner. A site whose configuration Plesk
// generated counts as a Plesk site without the Plesk source.
func refineVHosts(host *Host, items []VHost) {
	panels := map[string]VHost{}
	for _, item := range items {
		if item.Source == VHostSourcePlesk || item.Source == VHostSourceDirectAdmin {
			panels[item.Name] = item
		}
	}
	var accounts []passwdAccount
	accountsRead := false
	for index := range items {
		item := &items[index]
		panel, ok := panels[item.Name]
		// Plesk generates the web server configuration of its sites, so
		// they are known even when the Plesk source fails.
		if !ok && item.ConfigFile != nil && pleskGenerated(*item.ConfigFile) {
			panel, ok = VHost{Source: VHostSourcePlesk, Name: item.Name}, true
		}
		if ok {
			names := append([]string{item.Name, panel.Name}, item.Aliases...)
			item.Aliases = slices.DeleteFunc(slices.Clone(item.Aliases), func(alias string) bool {
				return implicitAlias(panel, names, alias) && (item.Certificate == nil || !item.Certificate.covers(alias))
			})
			continue
		}
		if item.Owner != nil || item.DocumentRoot == nil {
			continue
		}
		if !accountsRead {
			accounts, accountsRead = passwdAccounts(host), true
		}
		item.Owner = homeOwner(accounts, *item.DocumentRoot)
	}
}

// pleskGenerated reports whether Plesk wrote a web server configuration
// file: /var/www/vhosts/system/<domain>/conf/*, which the web servers
// include through plesk.conf.d/vhosts/<domain>.conf links, and the
// plesk.conf.d/forwarding, wildcards and ip_default files.
func pleskGenerated(name string) bool {
	return strings.HasPrefix(name, pleskVhostsSystem+"/") || strings.Contains(name, "/plesk.conf.d/")
}

// pleskPreviewLabel is the address label of a Plesk preview name,
// example.com.192-0-2-1.plesk.page, for IPv4 and IPv6 addresses.
var pleskPreviewLabel = regexp.MustCompile(`^([0-9]{1,3}(-[0-9]{1,3}){3}|[0-9a-f]{0,4}(-[0-9a-f]{0,4}){2,7})\.`)

// implicitAlias reports whether a panel adds alias to the site on its own:
// Plesk's ipv4.<name> and ipv6.<name> names and its preview name
// <name>.<address>.<preview zone>, for the site's names and its domain
// aliases, and the www.<name> of a subdomain, which Plesk and DirectAdmin
// serve but rarely has DNS or a certificate.
func implicitAlias(panel VHost, names []string, alias string) bool {
	if panel.subdomain && alias == "www."+panel.Name {
		return true
	}
	if panel.Source != VHostSourcePlesk {
		return false
	}
	for _, name := range names {
		if alias == "ipv4."+name || alias == "ipv6."+name {
			return true
		}
		if rest, ok := strings.CutPrefix(alias, name+"."); ok && pleskPreviewLabel.MatchString(rest) {
			return true
		}
	}
	return false
}

// minimumUserUID is the first uid of regular users on Debian and EL.
const minimumUserUID = 1000

// homeOwner returns the regular user whose home directory holds root, the
// deepest when homes nest, or nil. Forge, Ploi and similar tools run each
// site as its own user from /home/<user>/<site>.
func homeOwner(accounts []passwdAccount, root string) *string {
	var owner *passwdAccount
	for index := range accounts {
		account := &accounts[index]
		home := strings.TrimSuffix(account.home, "/")
		if account.uid < minimumUserUID || account.uid == 65534 || home == "" || !strings.HasPrefix(root, home+"/") {
			continue
		}
		if owner == nil || len(home) > len(strings.TrimSuffix(owner.home, "/")) {
			owner = account
		}
	}
	if owner == nil {
		return nil
	}
	return &owner.name
}

// vhostRun holds per-collection caches shared by the sources.
type vhostRun struct {
	host         *Host
	store        *certificateStore
	certificates map[string]*VHostCertificate
	warnings     map[string][]string
	plesk        *pleskState
}

// warn records a non-fatal problem of a source, logged when it changes.
func (run *vhostRun) warn(source, message string) {
	run.warnings[source] = append(run.warnings[source], message)
}

// addWWWAliases adds the www.<name> alias that Plesk and DirectAdmin
// always serve, unless the name already starts with www. or is a wildcard.
func addWWWAliases(items []VHost) []VHost {
	for index := range items {
		item := &items[index]
		if item.Name == "" || strings.HasPrefix(item.Name, "www.") || strings.HasPrefix(item.Name, "*") {
			continue
		}
		item.Aliases = append(slices.Clone(item.Aliases), "www."+item.Name)
	}
	return items
}

// mergeVHosts sets source on every item, names a site that serves both
// www.<name> and <name> by the latter, folds redirects into the sites they
// point to (see foldRedirects) and folds items with the same name
// into one: ports and aliases are joined, ssl is true when any block
// serves TLS, and single-valued fields come from the first TLS block that
// has them, then from the first plain block. Running it again on its
// result changes nothing.
func mergeVHosts(source string, items []VHost) []VHost {
	var sites, redirects []VHost
	for _, item := range items {
		item.Source = source
		item.Name = strings.TrimSuffix(item.Name, ".")
		if item.Name == "" {
			continue
		}
		if item.Redirect != nil {
			redirects = append(redirects, item)
		} else {
			sites = append(sites, item)
		}
	}
	result := mergeByName(preferApex(mergeByName(sites)))
	rest := mergeByName(foldRedirects(result, redirects))
	// Folding can add the apex of a www site, as Plesk's redirect to the
	// preferred www name does.
	result = append(mergeByName(preferApex(result)), rest...)
	for position := range result {
		item := &result[position]
		item.Aliases = slices.DeleteFunc(sortedUnique(item.Aliases), func(alias string) bool { return alias == item.Name || alias == "" })
		item.Ports = sortedUnique(item.Ports)
	}
	return result
}

// mergeByName folds items with the same name into one, TLS blocks first.
func mergeByName(items []VHost) []VHost {
	ordered := slices.Clone(items)
	slices.SortStableFunc(ordered, func(a, b VHost) int {
		switch {
		case a.SSL == b.SSL:
			return 0
		case a.SSL:
			return -1
		default:
			return 1
		}
	})
	index := make(map[string]int)
	var result []VHost
	for _, item := range ordered {
		position, seen := index[item.Name]
		if !seen {
			index[item.Name] = len(result)
			item.Aliases = slices.Clone(item.Aliases)
			item.Ports = slices.Clone(item.Ports)
			result = append(result, item)
			continue
		}
		merged := &result[position]
		merged.Aliases = append(merged.Aliases, item.Aliases...)
		merged.Ports = append(merged.Ports, item.Ports...)
		merged.SSL = merged.SSL || item.SSL
		merged.HTTP2 = merged.HTTP2 || item.HTTP2
		merged.HTTP3 = merged.HTTP3 || item.HTTP3
		merged.HTTPSRedirect = merged.HTTPSRedirect || item.HTTPSRedirect
		if merged.DocumentRoot == nil {
			merged.DocumentRoot = item.DocumentRoot
		}
		if merged.PHP == nil {
			merged.PHP = item.PHP
		}
		if merged.Certificate == nil {
			merged.Certificate = item.Certificate
		}
		if merged.ConfigFile == nil {
			merged.ConfigFile = item.ConfigFile
		}
		if merged.Owner == nil {
			merged.Owner = item.Owner
		}
		if merged.Redirect == nil {
			merged.Redirect = item.Redirect
		}
	}
	return result
}

// preferApex names a site that serves both www.<name> and <name> by the
// latter, as its panel and other sources name it, whichever its web server
// configuration lists first.
func preferApex(items []VHost) []VHost {
	for index := range items {
		item := &items[index]
		apex, ok := strings.CutPrefix(item.Name, "www.")
		if !ok {
			continue
		}
		if position := slices.Index(item.Aliases, apex); position >= 0 {
			item.Aliases[position] = item.Name
			item.Name = apex
		}
	}
	return items
}

// foldRedirects folds the blocks that only redirect into the sites they
// redirect to, and returns the rest. Hosting panels and ACME clients add
// such blocks to every site (Forge, Ploi, certbot, Plesk):
//
//   - A redirect to https on the requested host (server_name .example.com;
//     return 301 https://$host$request_uri) adds its ports to the sites of
//     its names and sets their https_redirect. Wildcard names are dropped
//     once one of its names is a site: they only answer plain HTTP.
//   - A redirect to another host (www.example.com to example.com) adds its
//     names to the target site as aliases, with its ports, when it serves
//     the target's certificate or none. A plain HTTP block's names the
//     target's certificate does not cover are dropped: they never reach
//     TLS, so they are not the certificate's concern.
//
// A redirect to a host the source does not serve, or with a certificate of
// its own, stays an item of its own with redirect set. Redirects to another
// host fold first, so the names they add are a site's when an HTTPS
// redirect for them folds, whatever order the configuration lists them in.
func foldRedirects(sites, redirects []VHost) []VHost {
	index := make(map[string]int)
	for position, site := range sites {
		index[site.Name] = position
	}
	for position, site := range sites {
		for _, alias := range site.Aliases {
			if _, taken := index[alias]; !taken {
				index[alias] = position
			}
		}
	}
	ordered := slices.Clone(redirects)
	slices.SortStableFunc(ordered, func(a, b VHost) int {
		switch {
		case (a.Redirect.Host == nil) == (b.Redirect.Host == nil):
			return 0
		case a.Redirect.Host != nil:
			return -1
		default:
			return 1
		}
	})
	var rest []VHost
	for _, redirect := range ordered {
		upgrade := redirect.Redirect.Host == nil && redirect.Redirect.https && redirect.Certificate == nil
		// A name a site serves folds into it whatever the redirect does,
		// so every name stays one item.
		var unmatched []string
		matched := false
		for _, name := range append([]string{redirect.Name}, redirect.Aliases...) {
			position, ok := index[name]
			if !ok {
				unmatched = append(unmatched, name)
				continue
			}
			site := &sites[position]
			site.Ports = append(site.Ports, redirect.Ports...)
			site.HTTPSRedirect = site.HTTPSRedirect || upgrade
			matched = true
		}
		if matched && upgrade {
			unmatched = slices.DeleteFunc(unmatched, isHostnamePattern)
		}
		if len(unmatched) == 0 {
			continue
		}
		if redirect.Redirect.Host == nil {
			rest = append(rest, withNames(redirect, unmatched))
			continue
		}
		position, ok := index[*redirect.Redirect.Host]
		if !ok || (redirect.Certificate != nil && (sites[position].Certificate == nil || sites[position].Certificate.Fingerprint != redirect.Certificate.Fingerprint)) {
			rest = append(rest, withNames(redirect, unmatched))
			continue
		}
		site := &sites[position]
		site.Ports = append(site.Ports, redirect.Ports...)
		for _, name := range unmatched {
			if redirect.Certificate != nil || site.Certificate == nil || site.Certificate.covers(name) {
				site.Aliases = append(site.Aliases, name)
				if _, taken := index[name]; !taken {
					index[name] = position
				}
			}
		}
	}
	return rest
}

// withNames returns a copy of item with the first plain name as its name
// and the others as aliases.
func withNames(item VHost, names []string) VHost {
	primary := slices.IndexFunc(names, func(name string) bool { return !isHostnamePattern(name) })
	if primary < 0 {
		primary = 0
	}
	item.Name = names[primary]
	item.Aliases = slices.Delete(slices.Clone(names), primary, primary+1)
	return item
}

// isHostnamePattern reports whether a server name is a wildcard
// (*.example.com, www.example.*) or a regular expression (~^…).
func isHostnamePattern(name string) bool {
	return strings.HasPrefix(name, "~") || strings.Contains(name, "*")
}

// covers reports whether the certificate is valid for a host name: one of
// its names or its subject is the name, or a wildcard matching exactly one
// label of it.
func (certificate *VHostCertificate) covers(host string) bool {
	host = strings.ToLower(host)
	for _, name := range append(slices.Clone(certificate.Names), strings.ToLower(certificate.Subject)) {
		if name == host {
			return true
		}
		suffix, wildcard := strings.CutPrefix(name, "*")
		if !wildcard || !strings.HasPrefix(suffix, ".") || strings.HasPrefix(host, "*") {
			continue
		}
		if label, ok := strings.CutSuffix(host, suffix); ok && label != "" && !strings.Contains(label, ".") {
			return true
		}
	}
	return false
}

// redirectHost returns the host of a URL after its scheme: a variable
// ($host, %{HTTP_HOST}) as written, or a literal host without its port.
func redirectHost(rest string) string {
	switch {
	case strings.HasPrefix(rest, "%{"):
		if end := strings.IndexByte(rest, '}'); end >= 0 {
			return rest[:end+1]
		}
		return ""
	case strings.HasPrefix(rest, "$"):
		end := 1
		for end < len(rest) && (rest[end] == '_' || rest[end] >= 'a' && rest[end] <= 'z' || rest[end] >= 'A' && rest[end] <= 'Z' || rest[end] >= '0' && rest[end] <= '9') {
			end++
		}
		return rest[:end]
	}
	authority := rest
	if end := strings.IndexAny(rest, "/?#$%"); end >= 0 {
		authority = rest[:end]
	}
	if _, after, found := strings.Cut(authority, "@"); found {
		authority = after
	}
	if strings.HasPrefix(authority, "[") {
		if end := strings.IndexByte(authority, ']'); end >= 0 {
			return authority[:end+1]
		}
		return ""
	}
	if colon := strings.LastIndexByte(authority, ':'); colon >= 0 {
		return authority[:colon]
	}
	return authority
}

// newRedirect describes a redirect to target, or returns nil when status
// is not a redirect or target is not an absolute URL. sameHost are the
// variables that stand for the requested host ($host, %{HTTP_HOST}).
func newRedirect(status int, target string, sameHost ...string) *VHostRedirect {
	switch status {
	case 301, 302, 303, 307, 308:
	default:
		return nil
	}
	scheme, rest, ok := strings.Cut(target, "://")
	if !ok || scheme == "" {
		return nil
	}
	hostname := redirectHost(rest)
	if hostname == "" {
		return nil
	}
	redirect := &VHostRedirect{Status: status, Target: target, https: strings.EqualFold(scheme, "https")}
	if !slices.ContainsFunc(sameHost, func(variable string) bool { return strings.EqualFold(variable, hostname) }) {
		redirect.Host = optionalString(strings.TrimSuffix(strings.ToLower(hostname), "."))
	}
	return redirect
}

// certificate returns the public facts of the first certificate in a PEM
// file, or nil. chain is the configured chain file, if any, and managed
// what the panel says about renewal. Results are cached per path and chain
// for the collection, and the parsed files across collections.
func (run *vhostRun) certificate(source, name, chain string, managed *CertificateManager) *VHostCertificate {
	if name == "" || strings.Contains(name, "$") {
		return nil
	}
	if strings.Contains(chain, "$") {
		chain = ""
	}
	key := name + "\x00" + chain
	if cached, ok := run.certificates[key]; ok {
		return cached
	}
	file := run.store.load(run.host, name)
	if warning := certificateWarning(name, file.err); warning != "" {
		run.warn(source, warning)
	}
	var certificate *VHostCertificate
	if leaf := file.leaf(); leaf != nil {
		certificate = vhostCertificate(leaf)
		certificate.source = &certificateSource{path: name, chain: chain, managed: managed}
	}
	run.certificates[key] = certificate
	return certificate
}

// vhostCertificate summarises a leaf certificate for the vhosts dataset.
func vhostCertificate(leaf *x509.Certificate) *VHostCertificate {
	subject := leaf.Subject.CommonName
	if subject == "" {
		subject = leaf.Subject.String()
	}
	issuer := leaf.Issuer.CommonName
	if issuer == "" {
		issuer = leaf.Issuer.String()
	}
	names := make([]string, 0, len(leaf.DNSNames))
	for _, name := range leaf.DNSNames {
		names = append(names, strings.ToLower(name))
	}
	return &VHostCertificate{
		Subject:     subject,
		Issuer:      issuer,
		NotAfter:    leaf.NotAfter.UTC().Format(time.RFC3339),
		Names:       sortedUnique(names),
		Fingerprint: fingerprintOf(leaf),
	}
}

// phpVersionPattern finds major.minor in socket, pool and binary paths:
// php8.2-fpm.sock, php82, ea-php81, /opt/remi/php83, plesk-php74-fpm.
var phpVersionPattern = regexp.MustCompile(`php-?([5-9])\.?([0-9]{1,2})(?:[^0-9]|$)`)

// phpVersionFrom returns the PHP version embedded in value, or nil.
func phpVersionFrom(value string) *string {
	match := phpVersionPattern.FindStringSubmatch(strings.ToLower(value))
	if match == nil {
		return nil
	}
	version := match[1] + "." + match[2]
	return &version
}

func newPHP(version *string, handler string) *VHostPHP {
	php := &VHostPHP{Version: version}
	if handler != "" {
		php.Handler = &handler
	}
	return php
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// absoluteFrom resolves a configuration path relative to base.
func absoluteFrom(base, name string) string {
	if name == "" || path.IsAbs(name) {
		return name
	}
	return path.Join(base, name)
}

// lowerHostname lower-cases a server name unless it is a regular
// expression (nginx "~…"), whose case matters.
func lowerHostname(name string) string {
	if strings.HasPrefix(name, "~") {
		return name
	}
	return strings.ToLower(name)
}
