package collector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"slices"
	"strings"
)

// Repository types.
const (
	RepositoryDeb    = "deb"
	RepositoryDebSrc = "deb-src"
	RepositoryRPM    = "rpm"
)

// RepositoriesInfo is the payload of the repositories dataset, sent in
// full. Items are in configuration order (sources.list, then
// sources.list.d or yum.repos.d by file name, each file top to bottom),
// which is deterministic, so the payload has no Normalize.
type RepositoriesInfo struct {
	// Manager is apt, yum or dnf.
	Manager string       `json:"manager"`
	Items   []Repository `json:"items"`
}

// Repository is one configured package source. apt stanzas are expanded
// into one item per type, URI and suite.
type Repository struct {
	// ID is stable: "<suite>:<hash>" on apt (see aptRepositoryID), the
	// .repo section on rpm.
	ID      string `json:"id"`
	File    string `json:"file"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
	// URIs are the configured URIs without credentials or query strings:
	// apt's URI, or rpm's baseurls followed by a "mirrorlist:" or
	// "metalink:" URI.
	URIs []string `json:"uris"`
	// Suite and Components are apt's; Name is rpm's.
	Suite      string            `json:"suite,omitempty"`
	Components []string          `json:"components,omitempty"`
	Name       string            `json:"name,omitempty"`
	Signing    RepositorySigning `json:"signing"`
	// Transport is the scheme of the first URI, the method before a "+"
	// (mirror+https is mirror, tor+http is tor).
	Transport string `json:"transport"`
	// Packages counts the installed packages whose origins include ID.
	Packages int `json:"packages"`
}

// RepositorySigning says how a repository's signatures are checked.
type RepositorySigning struct {
	// SignedBy is apt's Signed-By (a keyring path or fingerprints; an
	// inline key is reduced to its fingerprints, or "inline") or rpm's
	// first gpgkey. Null means apt's global keyring, or no gpgkey.
	SignedBy *string `json:"signed_by"`
	// Trusted is apt's trusted=yes: signatures are not checked.
	Trusted bool `json:"trusted"`
	// GPGCheck is rpm's effective gpgcheck; null on apt.
	GPGCheck *bool `json:"gpgcheck"`
}

// Repository source locations. Nothing else under /etc/apt is read: in
// particular never auth.conf or auth.conf.d, which hold credentials.
var (
	aptSourcesList  = "/etc/apt/sources.list"
	aptSourcesParts = "/etc/apt/sources.list.d"
	aptListsDir     = "/var/lib/apt/lists"
	yumReposDir     = "/etc/yum.repos.d"
)

// maximumSourceFile caps a sources or .repo file; larger ones are skipped.
const maximumSourceFile = 1 << 20

// Repositories collects the repositories dataset. It shares the origin
// index with the packages collector, which reports per package the
// repositories counted here.
type Repositories struct {
	host    *Host
	origins *Origins
}

// NewRepositories returns the collector. origins is shared with
// NewPackages; nil uses a private one.
func NewRepositories(host *Host, origins *Origins) *Repositories {
	if origins == nil {
		origins = NewOrigins(host)
	}
	return &Repositories{host: host, origins: origins}
}

func (*Repositories) Name() string { return "repositories" }

// ChangeToken implements ChangeDetector with the entries of the apt
// sources and yum repository directories and apt's lists directory, which
// an apt-get update by someone else replaces.
func (collector *Repositories) ChangeToken() string {
	digest := fnv.New64a()
	treeDigest(digest, collector.host, []string{aptSourcesList, aptSourcesParts, yumReposDir}, 1, 1000)
	if info, err := fs.Stat(collector.host.FS, relative(aptListsDir)); err == nil {
		fmt.Fprintf(digest, "%s\x00%d\n", aptListsDir, info.ModTime().UnixNano())
	}
	return fmt.Sprintf("%016x", digest.Sum64())
}

func (collector *Repositories) Collect(ctx context.Context) (any, error) {
	host := collector.host
	manager := host.updateManager()
	if manager == "" {
		return nil, errors.New("no supported package manager (apt, dnf or yum) found")
	}
	if lock, blocking := host.packageLockBlocking(); blocking && manager != ManagerApt {
		return nil, &BusyError{Lock: lock.String()}
	}
	repositories := host.configuredRepositories(manager)
	if manager != ManagerApt {
		if err := host.applyRepolist(ctx, manager, repositories); err != nil {
			return nil, err
		}
	}
	index, err := collector.origins.resolve(ctx)
	if err != nil {
		return nil, err
	}
	if index.determined {
		counts := index.counts()
		for position := range repositories {
			repositories[position].Packages = counts[repositories[position].ID]
		}
	}
	info := &RepositoriesInfo{Manager: manager, Items: repositories}
	if info.Items == nil {
		info.Items = []Repository{}
	}
	return info, nil
}

// configuredRepositories reads the apt sources or the yum/dnf .repo files,
// deduplicated by id (the first definition wins).
func (host *Host) configuredRepositories(manager string) []Repository {
	var repositories []Repository
	if manager == ManagerApt {
		for _, entry := range host.aptEntries() {
			repositories = append(repositories, entry.repository())
		}
	} else {
		repositories = host.rpmRepositories(manager)
	}
	seen := make(map[string]bool, len(repositories))
	unique := repositories[:0]
	for _, repository := range repositories {
		if !seen[repository.ID] {
			seen[repository.ID] = true
			unique = append(unique, repository)
		}
	}
	return unique
}

// readSourceFile reads a small configuration file; large, missing or
// unreadable ones are skipped.
func (host *Host) readSourceFile(name string) ([]byte, bool) {
	info, err := fs.Stat(host.FS, relative(name))
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximumSourceFile {
		return nil, false
	}
	data, err := host.readFile(name)
	if err != nil {
		return nil, false
	}
	return data, true
}

// sourceFiles lists the regular files of directory whose names end in one
// of suffixes, sorted, as absolute paths. Names are restricted like apt
// does (letters, digits, "_", "-" and "."), which also skips editor
// backups.
func (host *Host) sourceFiles(directory string, suffixes ...string) []string {
	entries, err := host.readDir(directory)
	if err != nil {
		return nil
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !validSourceName(name) {
			continue
		}
		for _, suffix := range suffixes {
			if strings.HasSuffix(name, suffix) && len(name) > len(suffix) {
				files = append(files, directory+"/"+name)
				break
			}
		}
	}
	slices.Sort(files)
	return files
}

func validSourceName(name string) bool {
	for _, character := range name {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		case character == '_' || character == '-' || character == '.':
		default:
			return false
		}
	}
	return name != ""
}

// sanitizeURI removes the userinfo (user:password@), the query string and
// the fragment of a repository URI. Variables such as $releasever are left
// as they are.
func sanitizeURI(uri string) string {
	if index := strings.IndexAny(uri, "?#"); index >= 0 {
		uri = uri[:index]
	}
	scheme, rest, found := strings.Cut(uri, "://")
	if !found {
		return uri
	}
	// Like apt and libcurl, the authority ends at the first "/".
	end := strings.IndexByte(rest, '/')
	if end < 0 {
		end = len(rest)
	}
	if at := strings.LastIndexByte(rest[:end], '@'); at >= 0 {
		rest = rest[at+1:]
	}
	return scheme + "://" + rest
}

// mirrorQueryKeys are the query parameters kept on mirrorlist and
// metalink URIs: they identify the repository (metalink?repo=epel-9&
// arch=$basearch) and are not secret.
var mirrorQueryKeys = map[string]bool{
	"repo": true, "arch": true, "country": true, "protocol": true,
	"content": true, "infra": true, "release": true,
}

// sanitizeMirrorURI is sanitizeURI for a mirrorlist or metalink URI: it
// keeps the mirrorQueryKeys parameters, as written and in their order,
// and drops every other one.
func sanitizeMirrorURI(uri string) string {
	base := sanitizeURI(uri)
	_, query, found := strings.Cut(strings.SplitN(uri, "#", 2)[0], "?")
	if !found {
		return base
	}
	var kept []string
	for _, parameter := range strings.Split(query, "&") {
		key, _, _ := strings.Cut(parameter, "=")
		if mirrorQueryKeys[key] {
			kept = append(kept, parameter)
		}
	}
	if len(kept) == 0 {
		return base
	}
	return base + "?" + strings.Join(kept, "&")
}

// uriTransport returns the scheme of uri in lower case, with a method
// prefix such as mirror+ or tor+ taking precedence: "https", "mirror",
// "tor", "file", "cdrom"; "" when uri has no scheme.
func uriTransport(uri string) string {
	scheme, _, found := strings.Cut(uri, ":")
	if !found || scheme == "" {
		return ""
	}
	for _, character := range scheme {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '+' && character != '-' && character != '.' {
			return ""
		}
	}
	method, _, _ := strings.Cut(scheme, "+")
	return strings.ToLower(method)
}

// shortHash is the first 8 hex digits of the SHA-256 of the parts, each
// terminated by a NUL byte. Ids only need to be unique within a server.
func shortHash(parts ...string) string {
	digest := sha256.New()
	for _, part := range parts {
		digest.Write([]byte(part))
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))[:8]
}

func stringPointer(value string) *string { return &value }
