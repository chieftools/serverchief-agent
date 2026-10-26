package collector

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"fmt"
	"hash/fnv"
	"log"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"go.chief.tools/serverchief-agent/trust"
)

// Certificate users (used_by kinds).
const (
	CertificateUseSite    = "site"
	CertificateUsePostfix = "postfix"
	CertificateUseDovecot = "dovecot"
	CertificateUseExim    = "exim"
	CertificateUsePanel   = "panel"
	CertificateUseOther   = "other"
)

// Certificate managers (managed_by.tool) and renewal automation
// (managed_by.renewal).
const (
	CertificateToolCertbot     = "certbot"
	CertificateToolAcmeSh      = "acme.sh"
	CertificateToolPlesk       = "plesk"
	CertificateToolDirectAdmin = "directadmin"
	CertificateToolForge       = "forge"

	CertificateRenewalTimer = "timer"
	CertificateRenewalCron  = "cron"
	CertificateRenewalPanel = "panel"
	CertificateRenewalNone  = "none"
)

// CertificatesInfo is the payload of the certificates dataset, sent as a
// delta keyed by fingerprint. Each certificate appears once, however many
// sites and services use it.
type CertificatesInfo struct {
	Items []Certificate `json:"items"`
}

// Certificate is one leaf certificate. Only public certificate data is
// collected; private keys are never read.
type Certificate struct {
	// Fingerprint is the SHA-256 of the leaf's DER, lowercase hex.
	Fingerprint string `json:"fingerprint"`
	// Serial is the serial number in lowercase hex.
	Serial    string          `json:"serial"`
	Subject   CertificateName `json:"subject"`
	Issuer    CertificateName `json:"issuer"`
	NotBefore string          `json:"not_before"`
	NotAfter  string          `json:"not_after"`
	// Names are the DNS names (lower case) and IP addresses of the subject
	// alternative names, sorted.
	Names              []string         `json:"names"`
	Key                CertificateKey   `json:"key"`
	SignatureAlgorithm string           `json:"signature_algorithm"`
	SelfSigned         bool             `json:"self_signed"`
	Chain              CertificateChain `json:"chain"`
	// Paths are the files the certificate was found in, sorted.
	Paths     []string            `json:"paths"`
	UsedBy    []CertificateUse    `json:"used_by"`
	ManagedBy *CertificateManager `json:"managed_by"`
}

// CertificateName is the common name and first organization of a subject
// or issuer.
type CertificateName struct {
	CN  *string `json:"cn"`
	Org *string `json:"org"`
}

// CertificateKey describes the public key.
type CertificateKey struct {
	// Type is rsa, ecdsa or ed25519.
	Type  string  `json:"type"`
	Bits  int     `json:"bits"`
	Curve *string `json:"curve"`
}

// CertificateChain tells whether the leaf verifies to a trusted root with
// the intermediates configured next to it. Complete is null when that
// cannot be determined, such as for a self-signed certificate.
type CertificateChain struct {
	Complete      *bool `json:"complete"`
	Intermediates int   `json:"intermediates"`
}

// CertificateUse is a site (ref: the vhosts key source/name) or a service
// (ref: its name) that serves the certificate.
type CertificateUse struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// CertificateManager is the tool that issues and renews the certificate.
type CertificateManager struct {
	Tool *string `json:"tool"`
	// Name is the certbot lineage, acme.sh domain or panel certificate.
	Name *string `json:"name"`
	// Plugin is the authenticator (webroot, dns-cloudflare, dns_cf, …).
	Plugin    *string `json:"plugin"`
	Installer *string `json:"installer"`
	// CA is a slug derived from CAURL or the issuer.
	CA      *string `json:"ca"`
	CAURL   *string `json:"ca_url"`
	KeyType *string `json:"key_type"`
	// Renewal is timer, cron, panel or none.
	Renewal *string `json:"renewal"`
	// Config is the renewal configuration file.
	Config *string `json:"config"`
}

// CertificateKeyOf is the delta key: the fingerprint.
func CertificateKeyOf(item Certificate) string {
	return item.Fingerprint
}

// Normalize sorts items by fingerprint and the lists inside each item.
func (info *CertificatesInfo) Normalize() {
	if info.Items == nil {
		info.Items = []Certificate{}
	}
	for index := range info.Items {
		item := &info.Items[index]
		item.Names = sortedUnique(item.Names)
		item.Paths = sortedUnique(item.Paths)
		slices.SortFunc(item.UsedBy, compareUses)
		item.UsedBy = slices.Compact(item.UsedBy)
		if item.UsedBy == nil {
			item.UsedBy = []CertificateUse{}
		}
	}
	slices.SortFunc(info.Items, func(a, b Certificate) int { return strings.Compare(a.Fingerprint, b.Fingerprint) })
}

func compareUses(a, b CertificateUse) int {
	if order := strings.Compare(a.Kind, b.Kind); order != 0 {
		return order
	}
	return strings.Compare(a.Ref, b.Ref)
}

const (
	// certificateChangeDepth reaches /root/.acme.sh/<domain>/<file> and
	// /etc/letsencrypt/live/<lineage>/<file>.
	certificateChangeDepth = 2
	// certificateChangeLimit bounds the entries stat'ed per root and poll.
	certificateChangeLimit = 5000
	// maxCertificateDirectories bounds the certificate directories of the
	// last collection that are watched.
	maxCertificateDirectories = 512
)

// certificateChangeRoots are the renewal, mail and panel trees whose
// changes trigger an early collection, besides vhostChangeRoots and the
// directories of the certificate files found last time.
var certificateChangeRoots = []string{
	certbotRenewalDir,
	"/etc/letsencrypt/live",
	"/etc/postfix",
	"/etc/dovecot",
	"/etc/exim4",
	"/etc/exim",
	"/etc/exim.conf",
	"/var/lib/exim4/config.autogenerated",
	pleskPanelCertificate,
	directAdminConfig,
	// Forge adds a renewal job per certificate.
	"/etc/cron.d",
	"/etc/cron.daily",
	"/etc/cron.weekly",
	"/etc/cron.monthly",
}

// Certificates collects the certificates dataset: the certificates of the
// vhosts dataset's sites and of the mail and panel services, with the
// certbot, acme.sh, Plesk or DirectAdmin automation that renews them.
type Certificates struct {
	host   *Host
	vhosts *VHosts

	mu     sync.Mutex
	warned string
	// directories are the certificate directories of the last collection,
	// watched by ChangeToken.
	directories []string
}

// NewCertificates returns the collector. It finds the sites' certificates
// through vhosts, sharing its parsed certificate files; nil uses a private
// vhosts collector.
func NewCertificates(host *Host, vhosts *VHosts) *Certificates {
	if vhosts == nil {
		vhosts = NewVHosts(host)
	}
	return &Certificates{host: host, vhosts: vhosts}
}

func (*Certificates) Name() string { return "certificates" }

// DeltaKeyFields implements KeyedDataset.
func (*Certificates) DeltaKeyFields() []string { return []string{"fingerprint"} }

// ChangeToken implements ChangeDetector with the entries of the web server,
// panel, mail, certbot and acme.sh trees and the directories that held
// certificates last time.
func (collector *Certificates) ChangeToken() string {
	collector.mu.Lock()
	directories := slices.Clone(collector.directories)
	collector.mu.Unlock()
	host := collector.host
	digest := fnv.New64a()
	treeDigest(digest, host, vhostChangeRoots, vhostChangeDepth, vhostChangeLimit)
	roots := append(slices.Clone(certificateChangeRoots), acmeShHome(host))
	treeDigest(digest, host, roots, certificateChangeDepth, certificateChangeLimit)
	treeDigest(digest, host, directories, 1, certificateChangeLimit)
	return fmt.Sprintf("%016x", digest.Sum64())
}

func (collector *Certificates) Collect(ctx context.Context) (any, error) {
	collector.mu.Lock()
	defer collector.mu.Unlock()

	run := newCertificateRun(collector.host, collector.vhosts.store)
	sites, err := collector.vhosts.recent(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		run.warn("sites: " + err.Error())
	}
	for _, site := range sites {
		if site.Certificate != nil && site.Certificate.source != nil {
			run.add(*site.Certificate.source, CertificateUse{Kind: CertificateUseSite, Ref: VHostKey(site)})
		}
	}
	for _, service := range serviceCertificates(ctx, run) {
		run.add(certificateSource{path: service.path, chain: service.chain}, CertificateUse{Kind: service.kind, Ref: service.ref})
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	managers := run.managers()

	info := &CertificatesInfo{Items: make([]Certificate, 0, len(run.entries))}
	var directories []string
	for fingerprint, entry := range run.entries {
		item := certificateItem(entry.leaf)
		item.Chain = entry.chain
		item.Paths = entry.paths
		item.UsedBy = entry.uses
		item.ManagedBy = pickManager(managers[fingerprint], entry.hints)
		completeManager(item.ManagedBy, entry.leaf)
		info.Items = append(info.Items, item)
		for _, name := range entry.paths {
			directories = append(directories, path.Dir(name))
		}
	}
	info.Normalize()

	directories = sortedUnique(directories)
	if len(directories) > maxCertificateDirectories {
		directories = directories[:maxCertificateDirectories]
	}
	collector.directories = directories
	if warnings := strings.Join(sortedUnique(run.warnings), "; "); warnings != collector.warned {
		if warnings != "" {
			log.Printf("[collector] certificates: %s", warnings)
		}
		collector.warned = warnings
	}
	return info, nil
}

// certificateRun gathers the certificates of one collection.
type certificateRun struct {
	host     *Host
	store    *certificateStore
	entries  map[string]*certificateEntry
	chains   map[string]CertificateChain
	warnings []string
	roots    *x509.CertPool
	rootsSet bool
	// renewal detection, computed on first use
	certbotRenewal *string
	acmeShRenewal  *string
	crontabs       []string
	crontabsRead   bool
}

// certificateEntry is one leaf and everything known about it.
type certificateEntry struct {
	leaf  *x509.Certificate
	paths []string
	uses  []CertificateUse
	chain CertificateChain
	// chained is set once a chain result was merged in.
	chained bool
	hints   []*CertificateManager
}

func newCertificateRun(host *Host, store *certificateStore) *certificateRun {
	return &certificateRun{host: host, store: store, entries: make(map[string]*certificateEntry), chains: make(map[string]CertificateChain)}
}

func (run *certificateRun) warn(message string) {
	run.warnings = append(run.warnings, message)
}

// load reads a certificate file through the shared store, recording
// unexpected problems as warnings.
func (run *certificateRun) load(name string) *certificateFile {
	file := run.store.load(run.host, name)
	if warning := certificateWarning(name, file.err); warning != "" {
		run.warn(warning)
	}
	return file
}

// add records a use of the certificate configured by source.
func (run *certificateRun) add(source certificateSource, use CertificateUse) {
	certificates := source.inline
	if source.path != "" {
		if strings.Contains(source.path, "$") {
			return
		}
		// One file configured as /etc/postfix/../ssl/x.pem and
		// /etc/ssl/x.pem is one path.
		source.path = path.Clean(source.path)
		certificates = run.load(source.path).certificates
	}
	if len(certificates) == 0 {
		return
	}
	leaf := certificates[0]
	extra := slices.Concat(certificates[1:], source.inlineChain)
	if source.chain != "" && !strings.Contains(source.chain, "$") {
		extra = append(extra, run.load(source.chain).certificates...)
	}
	fingerprint := fingerprintOf(leaf)
	entry := run.entries[fingerprint]
	if entry == nil {
		entry = &certificateEntry{leaf: leaf}
		run.entries[fingerprint] = entry
	}
	if source.path != "" {
		entry.paths = append(entry.paths, source.path)
	}
	entry.uses = append(entry.uses, use)
	if source.managed != nil {
		entry.hints = append(entry.hints, source.managed)
	}
	chain := run.chain(leaf, extra)
	if !entry.chained {
		entry.chain, entry.chained = chain, true
	} else {
		entry.chain = mergeChains(entry.chain, chain)
	}
}

// mergeChains combines the results of the places a certificate is
// configured: complete when any of them is, the most intermediates.
func mergeChains(a, b CertificateChain) CertificateChain {
	merged := CertificateChain{Intermediates: max(a.Intermediates, b.Intermediates)}
	switch {
	case a.Complete != nil && *a.Complete, b.Complete != nil && *b.Complete:
		merged.Complete = boolPointer(true)
	case a.Complete != nil || b.Complete != nil:
		merged.Complete = boolPointer(false)
	}
	return merged
}

func boolPointer(value bool) *bool {
	return &value
}

// trustedRoots is the system roots plus the embedded bundle, built once.
var trustedRoots = sync.OnceValue(trust.Roots)

// rootPool returns the host's trust anchors, or nil when unknown.
func (run *certificateRun) rootPool() *x509.CertPool {
	if !run.rootsSet {
		run.rootsSet = true
		if run.host.Roots != nil {
			run.roots = run.host.Roots()
		}
	}
	return run.roots
}

// chain verifies leaf against the trusted roots using the intermediates in
// extra (certificates of the same file and the configured chain file).
// Self-signed certificates in extra are roots, not intermediates, and are
// neither counted nor trusted. Verification happens at a moment inside the
// leaf's validity, so an expired certificate with a complete chain still
// reads as complete.
func (run *certificateRun) chain(leaf *x509.Certificate, extra []*x509.Certificate) CertificateChain {
	intermediates := x509.NewCertPool()
	seen := map[string]bool{fingerprintOf(leaf): true}
	var key strings.Builder
	key.WriteString(fingerprintOf(leaf))
	count := 0
	for _, certificate := range extra {
		fingerprint := fingerprintOf(certificate)
		if seen[fingerprint] || isSelfSigned(certificate) {
			continue
		}
		seen[fingerprint] = true
		intermediates.AddCert(certificate)
		key.WriteString("," + fingerprint)
		count++
	}
	result := CertificateChain{Intermediates: count}
	if cached, ok := run.chains[key.String()]; ok {
		return cached
	}
	roots := run.rootPool()
	if roots != nil && !isSelfSigned(leaf) {
		_, err := leaf.Verify(x509.VerifyOptions{
			Intermediates: intermediates,
			Roots:         roots,
			CurrentTime:   verificationTime(leaf, run.host.now()),
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		})
		result.Complete = boolPointer(err == nil)
	}
	run.chains[key.String()] = result
	return result
}

// verificationTime is now, clamped to the leaf's validity.
func verificationTime(leaf *x509.Certificate, now time.Time) time.Time {
	switch {
	case now.Before(leaf.NotBefore):
		return leaf.NotBefore
	case now.After(leaf.NotAfter):
		return leaf.NotAfter
	}
	return now
}

// isSelfSigned reports whether a certificate is issued by itself: the same
// subject and issuer, and a signature its own key verifies.
func isSelfSigned(certificate *x509.Certificate) bool {
	if !bytes.Equal(certificate.RawSubject, certificate.RawIssuer) {
		return false
	}
	return certificate.CheckSignature(certificate.SignatureAlgorithm, certificate.RawTBSCertificate, certificate.Signature) == nil
}

// certificateItem holds the facts of the leaf itself.
func certificateItem(leaf *x509.Certificate) Certificate {
	names := make([]string, 0, len(leaf.DNSNames)+len(leaf.IPAddresses))
	for _, name := range leaf.DNSNames {
		names = append(names, strings.ToLower(name))
	}
	for _, address := range leaf.IPAddresses {
		names = append(names, address.String())
	}
	serial := "00"
	if leaf.SerialNumber != nil && leaf.SerialNumber.Sign() != 0 {
		serial = fmt.Sprintf("%x", leaf.SerialNumber.Bytes())
		if leaf.SerialNumber.Sign() < 0 {
			serial = "-" + serial
		}
	}
	return Certificate{
		Fingerprint:        fingerprintOf(leaf),
		Serial:             serial,
		Subject:            certificateName(leaf.Subject.CommonName, leaf.Subject.Organization),
		Issuer:             certificateName(leaf.Issuer.CommonName, leaf.Issuer.Organization),
		NotBefore:          leaf.NotBefore.UTC().Format(time.RFC3339),
		NotAfter:           leaf.NotAfter.UTC().Format(time.RFC3339),
		Names:              names,
		Key:                certificateKey(leaf),
		SignatureAlgorithm: leaf.SignatureAlgorithm.String(),
		SelfSigned:         isSelfSigned(leaf),
	}
}

func certificateName(commonName string, organizations []string) CertificateName {
	name := CertificateName{CN: optionalString(commonName)}
	if len(organizations) > 0 {
		name.Org = optionalString(organizations[0])
	}
	return name
}

func certificateKey(leaf *x509.Certificate) CertificateKey {
	switch key := leaf.PublicKey.(type) {
	case *rsa.PublicKey:
		return CertificateKey{Type: "rsa", Bits: key.N.BitLen()}
	case *ecdsa.PublicKey:
		params := key.Curve.Params()
		return CertificateKey{Type: "ecdsa", Bits: params.BitSize, Curve: optionalString(params.Name)}
	case ed25519.PublicKey:
		return CertificateKey{Type: "ed25519", Bits: 256}
	}
	return CertificateKey{Type: strings.ToLower(leaf.PublicKeyAlgorithm.String())}
}

// pickManager prefers the renewal tool configured for the certificate
// (certbot, acme.sh) over what a panel says, and is deterministic when
// several claim it.
func pickManager(configured []*CertificateManager, hints []*CertificateManager) *CertificateManager {
	candidates := configured
	if len(candidates) == 0 {
		candidates = hints
	}
	if len(candidates) == 0 {
		return nil
	}
	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if managerSortKey(candidate) < managerSortKey(best) {
			best = candidate
		}
	}
	clone := *best
	return &clone
}

func managerSortKey(manager *CertificateManager) string {
	value := func(pointer *string) string {
		if pointer == nil {
			return ""
		}
		return *pointer
	}
	return value(manager.Tool) + "\x00" + value(manager.Config) + "\x00" + value(manager.Name)
}

// completeManager fills in the CA slug from the ACME directory URL, or else
// from the issuer.
func completeManager(manager *CertificateManager, leaf *x509.Certificate) {
	if manager == nil || manager.CA != nil {
		return
	}
	if manager.CAURL != nil {
		if ca := caFromURL(*manager.CAURL); ca != "" {
			manager.CA = &ca
			return
		}
	}
	organization := ""
	if len(leaf.Issuer.Organization) > 0 {
		organization = leaf.Issuer.Organization[0]
	}
	if ca := caFromIssuer(organization, leaf.Issuer.CommonName); ca != "" {
		manager.CA = &ca
	}
}

// caDirectories maps ACME directory hosts to CA slugs.
var caDirectories = map[string]string{
	"acme-v02.api.letsencrypt.org":         "letsencrypt",
	"acme-v01.api.letsencrypt.org":         "letsencrypt",
	"acme-staging-v02.api.letsencrypt.org": "letsencrypt-staging",
	"acme-staging.api.letsencrypt.org":     "letsencrypt-staging",
	"acme.zerossl.com":                     "zerossl",
	"api.buypass.com":                      "buypass",
	"api.test4.buypass.no":                 "buypass",
	"dv.acme-v02.api.pki.goog":             "google",
	"dv.acme-v02.test-api.pki.goog":        "google",
	"acme.sectigo.com":                     "sectigo",
	"acme-qa.secure.trust-provider.com":    "sectigo",
}

// caFromURL returns the CA slug of an ACME directory URL, or "".
func caFromURL(directory string) string {
	parsed, err := url.Parse(strings.TrimSpace(directory))
	if err != nil {
		return ""
	}
	return caDirectories[strings.ToLower(parsed.Hostname())]
}

// caFromIssuer returns the CA slug of an issuer organization, or "".
func caFromIssuer(organization, commonName string) string {
	lower := strings.ToLower(organization)
	switch {
	case strings.Contains(lower, "let's encrypt"), strings.Contains(lower, "lets encrypt"):
		if strings.Contains(lower, "staging") || strings.Contains(strings.ToLower(commonName), "staging") || strings.Contains(commonName, "(STAGING)") {
			return "letsencrypt-staging"
		}
		return "letsencrypt"
	case strings.Contains(lower, "zerossl"):
		return "zerossl"
	case strings.Contains(lower, "buypass"):
		return "buypass"
	case strings.Contains(lower, "google trust services"):
		return "google"
	case strings.Contains(lower, "sectigo"), strings.Contains(lower, "comodo"):
		return "sectigo"
	}
	return ""
}
