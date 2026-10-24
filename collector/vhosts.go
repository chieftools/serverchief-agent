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
	SSL          bool              `json:"ssl"`
	DocumentRoot *string           `json:"document_root"`
	PHP          *VHostPHP         `json:"php"`
	Certificate  *VHostCertificate `json:"certificate"`
	// ConfigFile is the file that defines the site (without a line number,
	// so editing other sites in the same file does not change the item).
	ConfigFile *string `json:"config_file"`
	// Owner is the Plesk subscription or DirectAdmin user; null for plain
	// web servers.
	Owner *string `json:"owner"`
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
// configuration under /var/www/vhosts/system whenever a domain changes.
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

// vhostSnapshotMaxAge bounds how old a vhosts collection may be for the
// certificates dataset to reuse it instead of collecting again.
const vhostSnapshotMaxAge = 30 * time.Minute

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
// backend and add them back on the next run.
type VHosts struct {
	host *Host

	mu   sync.Mutex
	last map[string][]VHost
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
// under roots, up to depth levels deep and limit entries per root.
func treeDigest(digest io.Writer, host *Host, roots []string, depth, limit int) {
	for _, root := range roots {
		count := 0
		_ = fs.WalkDir(host.FS, relative(root), func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
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

// recent returns the items of a collection that is at most
// vhostSnapshotMaxAge old and whose configuration has not changed since,
// collecting again otherwise. The certificates dataset uses it to find the
// sites' certificates without running apachectl and plesk db again.
func (collector *VHosts) recent(ctx context.Context) ([]VHost, error) {
	token := collector.ChangeToken()
	collector.mu.Lock()
	snapshot := collector.snapshot
	collector.mu.Unlock()
	if snapshot != nil && snapshot.token == token && collector.host.now().Sub(snapshot.at) < vhostSnapshotMaxAge {
		return snapshot.items, nil
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

func (collector *VHosts) Collect(ctx context.Context) (any, error) {
	token := collector.ChangeToken()
	collector.mu.Lock()
	defer collector.mu.Unlock()

	run := &vhostRun{host: collector.host, store: collector.store, certificates: make(map[string]*VHostCertificate), warnings: make(map[string][]string)}
	info := &VHostsInfo{Items: []VHost{}}
	var failures []string
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
			failures = append(failures, source.name+": "+err.Error())
			if previous, ok := collector.last[source.name]; ok {
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
		return nil, errors.New(strings.Join(failures, "; "))
	}
	info.Normalize()
	collector.snapshot = &vhostSnapshot{token: token, at: collector.host.now(), items: slices.Clone(info.Items)}
	return info, nil
}

// vhostRun holds per-collection caches shared by the sources.
type vhostRun struct {
	host         *Host
	store        *certificateStore
	certificates map[string]*VHostCertificate
	warnings     map[string][]string
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

// mergeVHosts sets source on every item and folds items with the same name
// into one: ports and aliases are joined, ssl is true when any block serves
// TLS, and single-valued fields come from the first TLS block that has
// them, then from the first plain block (a port 80 block is often only a
// redirect to https).
func mergeVHosts(source string, items []VHost) []VHost {
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
		item.Source = source
		item.Name = strings.TrimSuffix(item.Name, ".")
		if item.Name == "" {
			continue
		}
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
	}
	for position := range result {
		item := &result[position]
		item.Aliases = slices.DeleteFunc(sortedUnique(item.Aliases), func(alias string) bool { return alias == item.Name || alias == "" })
		item.Ports = sortedUnique(item.Ports)
	}
	return result
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
