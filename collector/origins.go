package collector

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
)

// Origins finds the repositories that offer each installed package. The
// packages and repositories collectors share one instance, so the apt
// lists are read (or dnf is asked) once per change of the package
// database, the sources or the lists.
type Origins struct {
	host *Host

	mu     sync.Mutex
	cached *originIndex
	warned string
}

// NewOrigins returns an origin resolver for host.
func NewOrigins(host *Host) *Origins { return &Origins{host: host} }

// originIndex is one resolution.
type originIndex struct {
	token string
	// determined is false when origins cannot be told at all (no apt
	// lists yet, dnf failed): the packages' origins are then omitted.
	determined bool
	// byPackage maps name/arch to the sorted ids of the repositories that
	// offer the installed version. Packages without an entry have none.
	byPackage map[string][]string
}

// apply sets the origins of items; nothing when undetermined.
func (index *originIndex) apply(items []Package) {
	if !index.determined {
		return
	}
	for position := range items {
		origins := index.byPackage[items[position].Name+"/"+items[position].Arch]
		items[position].Origins = append([]string{}, origins...)
	}
}

// counts returns the number of installed packages per repository id.
func (index *originIndex) counts() map[string]int {
	counts := map[string]int{}
	for _, ids := range index.byPackage {
		for _, id := range ids {
			counts[id]++
		}
	}
	return counts
}

// resolve returns the current index, reusing the last one while its
// sources are unchanged. Only a held package-manager lock (BusyError) or
// a cancelled ctx is an error; any other failure is logged and leaves the
// origins undetermined.
func (origins *Origins) resolve(ctx context.Context) (*originIndex, error) {
	origins.mu.Lock()
	defer origins.mu.Unlock()
	host := origins.host
	manager := host.packageManager()
	token := origins.token(manager)
	if origins.cached != nil && origins.cached.token == token {
		return origins.cached, nil
	}
	var index *originIndex
	var err error
	switch manager {
	case ManagerDpkg:
		index, err = host.aptOrigins(ctx)
	case ManagerRPM:
		index, err = host.rpmOrigins(ctx)
	default:
		index = &originIndex{}
	}
	if err != nil {
		if errors.Is(err, ErrPackageManagerBusy) || ctx.Err() != nil {
			return nil, err
		}
		if message := err.Error(); message != origins.warned {
			log.Printf("[collector] package origins: %v; leaving them out", err)
			origins.warned = message
		}
		return &originIndex{}, nil // not cached: try again next time
	}
	origins.warned = ""
	index.token = token
	origins.cached = index
	return index, nil
}

// token is a cheap digest of everything an index depends on: the package
// database, the source and repository files and, for apt, the lists.
func (origins *Origins) token(manager string) string {
	host := origins.host
	digest := fnv.New64a()
	fmt.Fprintf(digest, "%s\x00%s\n", manager, host.packageDatabaseToken())
	if manager == ManagerDpkg {
		treeDigest(digest, host, []string{aptSourcesList, aptSourcesParts}, 1, 1000)
		if entries, err := host.readDir(aptListsDir); err == nil {
			for _, entry := range entries {
				if !strings.Contains(entry.Name(), "_Packages") {
					continue
				}
				if info, err := entry.Info(); err == nil {
					fmt.Fprintf(digest, "%s\x00%d\x00%d\n", entry.Name(), info.ModTime().UnixNano(), info.Size())
				}
			}
		}
	} else {
		treeDigest(digest, host, []string{yumReposDir}, 1, 1000)
	}
	return fmt.Sprintf("%016x", digest.Sum64())
}

// rpmOrigins asks dnf or yum which repository each installed package came
// from and keeps the ids of configured repositories: "@System",
// "anaconda", "installed" or a removed repository leave a package with
// none.
func (host *Host) rpmOrigins(ctx context.Context) (*originIndex, error) {
	manager := host.updateManager()
	if manager != ManagerDnf && manager != ManagerYum {
		return &originIndex{}, nil // plain rpm keeps no origins
	}
	if lock, blocking := host.packageLockBlocking(); blocking {
		return nil, &BusyError{Lock: lock.String()}
	}
	configured := map[string]bool{}
	for _, repository := range host.rpmRepositories(manager) {
		configured[repository.ID] = true
	}
	fromRepos, err := host.rpmFromRepos(ctx, manager)
	if err != nil {
		return nil, err
	}
	index := &originIndex{determined: true, byPackage: make(map[string][]string, len(fromRepos))}
	for key, repositories := range fromRepos {
		var ids []string
		for _, repository := range repositories {
			if configured[repository] {
				ids = append(ids, repository)
			}
		}
		if len(ids) > 0 {
			slices.Sort(ids)
			index.byPackage[key] = slices.Compact(ids)
		}
	}
	return index, nil
}

// aptIndexFile is a Packages list file and the repositories it belongs
// to.
type aptIndexFile struct {
	name        string
	compression string
	ids         []string
}

// Compression suffixes of list files. apt keeps them compressed with
// Acquire::GzipIndexes, as the Debian and Ubuntu container images do
// (lz4 since apt 1.1, gz before).
var (
	aptReadableCompression   = []string{".lz4", ".gz", ".bz2"}
	aptUnreadableCompression = []string{".xz", ".zst", ".lzma"}
)

// aptOrigins matches every installed name, version and arch against the
// Packages lists of the enabled deb sources. Without any Packages list
// (apt-get update never ran) the origins are undetermined.
func (host *Host) aptOrigins(ctx context.Context) (*originIndex, error) {
	listEntries, err := host.readDir(aptListsDir)
	if err != nil && !ignorable(err) {
		return nil, err
	}
	var matchers []aptIndexMatcher
	var ids []string
	for _, entry := range host.aptEntries() {
		if entry.kind == RepositoryDeb && entry.enabled {
			matchers = append(matchers, newAptIndexMatcher(entry))
			ids = append(ids, entry.id())
		}
	}
	var files []aptIndexFile
	anyIndex := false
	for _, entry := range listEntries {
		name := entry.Name()
		if entry.IsDir() || !strings.Contains(name, "_Packages") {
			continue
		}
		base, compression := name, ""
		for _, suffix := range slices.Concat(aptReadableCompression, aptUnreadableCompression) {
			if trimmed, found := strings.CutSuffix(name, suffix); found {
				base, compression = trimmed, suffix
				break
			}
		}
		if !strings.HasSuffix(base, "_Packages") {
			continue
		}
		anyIndex = true
		file := aptIndexFile{name: name, compression: compression}
		for position, matcher := range matchers {
			if matcher.matches(base) {
				file.ids = append(file.ids, ids[position])
			}
		}
		if len(file.ids) == 0 {
			continue // a list apt no longer uses
		}
		if slices.Contains(aptUnreadableCompression, compression) {
			return nil, fmt.Errorf("%s/%s: %s compression is not supported", aptListsDir, name, compression)
		}
		files = append(files, file)
	}
	if !anyIndex {
		return &originIndex{}, nil
	}

	data, err := host.readFile(dpkgStatusPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dpkgStatusPath, err)
	}
	type installedVersion struct{ arch, version string }
	installed := map[string][]installedVersion{}
	for _, item := range collapsePackages(parseDpkgStatus(data), compareDebianVersions) {
		installed[item.Name] = append(installed[item.Name], installedVersion{item.Arch, item.Version})
	}
	index := &originIndex{determined: true, byPackage: map[string][]string{}}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := host.scanAptIndex(ctx, file, func(name, version, arch []byte) {
			versions, found := installed[string(name)]
			if !found {
				return
			}
			for _, candidate := range versions {
				if candidate.arch == string(arch) && candidate.version == string(version) {
					key := string(name) + "/" + candidate.arch
					for _, id := range file.ids {
						index.byPackage[key] = appendUnique(index.byPackage[key], id)
					}
				}
			}
		})
		if err != nil {
			return nil, fmt.Errorf("%s/%s: %w", aptListsDir, file.name, err)
		}
	}
	for key := range index.byPackage {
		slices.Sort(index.byPackage[key])
	}
	return index, nil
}

// maximumIndexLine caps a line of a Packages file, and with it the memory
// of a scan.
const maximumIndexLine = 1 << 20

// scanAptIndex streams a Packages list and calls visit with the Package,
// Version and Architecture of each stanza. The slices are only valid
// during the call.
func (host *Host) scanAptIndex(ctx context.Context, file aptIndexFile, visit func(name, version, arch []byte)) error {
	handle, err := host.FS.Open(relative(aptListsDir + "/" + file.name))
	if err != nil {
		return err
	}
	defer handle.Close()
	var reader io.Reader = handle
	switch file.compression {
	case ".gz":
		gz, err := gzip.NewReader(handle)
		if err != nil {
			return err
		}
		defer gz.Close()
		reader = gz
	case ".bz2":
		reader = bzip2.NewReader(handle)
	case ".lz4":
		reader = newLZ4Reader(handle)
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), maximumIndexLine)
	var name, version, arch []byte
	flush := func() {
		if len(name) > 0 && len(version) > 0 && len(arch) > 0 {
			visit(name, version, arch)
		}
		name, version, arch = name[:0], version[:0], arch[:0]
	}
	count := 0
	for scanner.Scan() {
		if count++; count%65536 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		line := bytes.TrimRight(scanner.Bytes(), "\r")
		switch {
		case len(line) == 0:
			flush()
		case line[0] == ' ' || line[0] == '\t':
		case bytes.HasPrefix(line, []byte("Package:")):
			name = append(name[:0], bytes.TrimSpace(line[len("Package:"):])...)
		case bytes.HasPrefix(line, []byte("Version:")):
			version = append(version[:0], bytes.TrimSpace(line[len("Version:"):])...)
		case bytes.HasPrefix(line, []byte("Architecture:")):
			arch = append(arch[:0], bytes.TrimSpace(line[len("Architecture:"):])...)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	flush()
	return nil
}
