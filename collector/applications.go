package collector

import (
	"context"
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
)

// Application kinds, the first that matches.
const (
	ApplicationKindWordPress = "wordpress"
	ApplicationKindLaravel   = "laravel"
	ApplicationKindSymfony   = "symfony"
	ApplicationKindComposer  = "composer"
	ApplicationKindNode      = "node"
)

// Package ecosystems of an application's dependencies.
const (
	EcosystemComposer = "composer"
	EcosystemNPM      = "npm"
)

const (
	// maxApplications bounds the applications of one server.
	maxApplications = 1000
	// maxApplicationEntries bounds the packages, plugins and themes of all
	// applications together.
	maxApplicationEntries = 100_000
	// maxLockfilePackages bounds the packages kept from one lockfile.
	maxLockfilePackages = 5000
	// maxWordPressExtensions bounds the plugins and themes of one install.
	maxWordPressExtensions = 500
	// maxLockfileSize bounds a lockfile or manifest that is read.
	maxLockfileSize = 64 << 20
	// applicationLevelsUp is how far above a document root the application
	// root is looked for: a Laravel public/ or a Bedrock web/ directory.
	applicationLevelsUp = 3
	// wordPressHeaderBytes is how much of a plugin or theme file WordPress
	// reads for its header (get_file_data).
	wordPressHeaderBytes = 8 << 10
	// maxPluginFiles bounds the PHP files of one plugin directory whose
	// header is read.
	maxPluginFiles = 64
	// applicationChangeLimit bounds the entries stat'ed per watched
	// directory and poll.
	applicationChangeLimit = 2000
)

// lockfiles are the dependency lockfiles read at an application root, in
// the order they are parsed.
var lockfiles = []string{"composer.lock", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock"}

// applicationMarkers make a directory an application root, besides a
// WordPress core.
var applicationMarkers = append([]string{"composer.json", "package.json", "artisan"}, lockfiles...)

// wordPressLayouts are where WordPress's core lives relative to the
// application root, with its content directory: a classic install, core
// in its own directory and Bedrock.
var wordPressLayouts = []struct{ core, content string }{
	{"", "wp-content"},
	{"wp", "wp-content"},
	{"wordpress", "wp-content"},
	{"web/wp", "web/app"},
}

// ApplicationsInfo is the payload of the applications dataset, sent as a
// delta keyed by path. Truncated is set when applications or entries were
// left out because a limit was reached.
type ApplicationsInfo struct {
	Truncated bool          `json:"truncated"`
	Items     []Application `json:"items"`
}

// Application is a web application a site serves: the directory at or
// above a document root with dependency manifests or a WordPress core.
// Only names and versions are collected; configuration files such as
// wp-config.php and .env are never read.
type Application struct {
	// Path is the application root as configured, through any symbolic
	// link (a deploy's current/), so a new release keeps its key.
	Path string `json:"path"`
	Kind string `json:"kind"`
	// Version is the WordPress, Laravel or Symfony version, null for other
	// kinds or when it cannot be read.
	Version *string `json:"version"`
	// Sites are the vhosts keys (source/name) of the sites it serves.
	Sites []string `json:"sites"`
	// Owner is the owner of the first site that has one.
	Owner *string `json:"owner"`
	// Lockfiles are the lockfiles read, by file name.
	Lockfiles []string             `json:"lockfiles"`
	Packages  []ApplicationPackage `json:"packages"`
	WordPress *WordPressInfo       `json:"wordpress"`
	// Truncated is set when packages, plugins or themes were left out.
	Truncated bool `json:"truncated"`
}

// ApplicationPackage is one dependency version from a lockfile.
type ApplicationPackage struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	// Direct is set when the manifest requires the package itself.
	Direct bool `json:"direct"`
	// Dev is set when only development dependencies need the package.
	Dev bool `json:"dev"`
}

// WordPressInfo holds the plugins and themes of a WordPress install, read
// from their headers like WordPress lists them. Whether they are active is
// in the database and not collected.
type WordPressInfo struct {
	Plugins []WordPressPlugin `json:"plugins"`
	Themes  []WordPressTheme  `json:"themes"`
}

// WordPressPlugin is a plugin directory or file. MustUse plugins
// (mu-plugins) are always active.
type WordPressPlugin struct {
	Slug    string  `json:"slug"`
	Name    *string `json:"name"`
	Version *string `json:"version"`
	MustUse bool    `json:"must_use"`
}

// WordPressTheme is a theme directory; Parent is the Template header of a
// child theme.
type WordPressTheme struct {
	Slug    string  `json:"slug"`
	Name    *string `json:"name"`
	Version *string `json:"version"`
	Parent  *string `json:"parent"`
}

// ApplicationKey is the delta key: the path.
func ApplicationKey(item Application) string {
	return item.Path
}

// Normalize sorts items by path and the lists inside each item.
func (info *ApplicationsInfo) Normalize() {
	if info.Items == nil {
		info.Items = []Application{}
	}
	for index := range info.Items {
		item := &info.Items[index]
		item.Sites = sortedUnique(item.Sites)
		if item.Sites == nil {
			item.Sites = []string{}
		}
		if item.Lockfiles == nil {
			item.Lockfiles = []string{}
		}
		if item.Packages == nil {
			item.Packages = []ApplicationPackage{}
		}
		slices.SortFunc(item.Packages, func(a, b ApplicationPackage) int {
			return cmpStrings(a.Ecosystem, b.Ecosystem, a.Name, b.Name, a.Version, b.Version)
		})
		if wordpress := item.WordPress; wordpress != nil {
			if wordpress.Plugins == nil {
				wordpress.Plugins = []WordPressPlugin{}
			}
			if wordpress.Themes == nil {
				wordpress.Themes = []WordPressTheme{}
			}
			slices.SortFunc(wordpress.Plugins, func(a, b WordPressPlugin) int {
				return cmpStrings(boolKey(a.MustUse), boolKey(b.MustUse), a.Slug, b.Slug)
			})
			slices.SortFunc(wordpress.Themes, func(a, b WordPressTheme) int { return strings.Compare(a.Slug, b.Slug) })
		}
	}
	slices.SortFunc(info.Items, func(a, b Application) int { return strings.Compare(a.Path, b.Path) })
}

// cmpStrings compares pairs of strings in order: a1 with b1, then a2 with
// b2 and so on.
func cmpStrings(pairs ...string) int {
	for index := 0; index+1 < len(pairs); index += 2 {
		if order := strings.Compare(pairs[index], pairs[index+1]); order != 0 {
			return order
		}
	}
	return 0
}

func boolKey(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

// Applications collects the applications dataset from the document roots
// of the vhosts dataset's sites. It never walks down into an application:
// nested lockfiles of plugins, vendored packages, backups and old deploy
// releases are not read.
type Applications struct {
	host   *Host
	vhosts *VHosts

	mu     sync.Mutex
	warned string
	// watched are the files and directories of the last collection whose
	// changes trigger an early collection.
	watched []string
}

// NewApplications returns the collector. It finds the document roots
// through vhosts; nil uses a private vhosts collector.
func NewApplications(host *Host, vhosts *VHosts) *Applications {
	if vhosts == nil {
		vhosts = NewVHosts(host)
	}
	return &Applications{host: host, vhosts: vhosts}
}

func (*Applications) Name() string { return "applications" }

// DeltaKeyFields implements KeyedDataset.
func (*Applications) DeltaKeyFields() []string { return []string{"path"} }

// ChangeToken implements ChangeDetector with the web server and panel
// configuration trees and the lockfiles, manifests, WordPress version file
// and plugin and theme directories of the last collection.
func (collector *Applications) ChangeToken() string {
	collector.mu.Lock()
	watched := slices.Clone(collector.watched)
	collector.mu.Unlock()
	digest := fnv.New64a()
	treeDigest(digest, collector.host, vhostChangeRoots, vhostChangeDepth, vhostChangeLimit)
	treeDigest(digest, collector.host, watched, 1, applicationChangeLimit)
	return fmt.Sprintf("%016x", digest.Sum64())
}

func (collector *Applications) Collect(ctx context.Context) (any, error) {
	collector.mu.Lock()
	defer collector.mu.Unlock()

	sites, err := collector.vhosts.recent(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if sites == nil {
			return nil, fmt.Errorf("sites: %w", err)
		}
	}

	run := &applicationRun{host: collector.host, roots: map[string]*Application{}}
	for _, site := range sites {
		if site.DocumentRoot == nil {
			continue
		}
		root := run.applicationRoot(*site.DocumentRoot)
		if root == "" {
			continue
		}
		item, seen := run.roots[root]
		if !seen {
			if len(run.roots) >= maxApplications {
				run.truncated = true
				continue
			}
			item = &Application{Path: root}
			run.roots[root] = item
			run.order = append(run.order, root)
		}
		item.Sites = append(item.Sites, VHostKey(site))
		if item.Owner == nil && site.Owner != nil {
			item.Owner = site.Owner
		}
	}

	slices.Sort(run.order)
	info := &ApplicationsInfo{Items: make([]Application, 0, len(run.order))}
	for _, root := range run.order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item := run.roots[root]
		run.inspect(item)
		info.Items = append(info.Items, *item)
	}
	info.Truncated = run.truncated
	info.Normalize()

	collector.watched = sortedUnique(run.watched)
	if warnings := strings.Join(sortedUnique(run.warnings), "; "); warnings != collector.warned {
		if warnings != "" {
			log.Printf("[collector] applications: %s", warnings)
		}
		collector.warned = warnings
	}
	return info, nil
}

// applicationRun gathers the applications of one collection.
type applicationRun struct {
	host      *Host
	roots     map[string]*Application
	order     []string
	entries   int
	truncated bool
	watched   []string
	warnings  []string
}

func (run *applicationRun) warn(format string, args ...any) {
	run.warnings = append(run.warnings, fmt.Sprintf(format, args...))
}

// applicationRoot returns the application root of a document root, "" when
// none of the directories at or up to applicationLevelsUp above it has a
// marker. The nearest directory with a marker wins, except that a
// WordPress directory without a composer.json defers to a parent that has
// one (Bedrock's web/). The walk never leaves the hosting user's tree and
// never reaches a top-level directory such as /var or /opt.
func (run *applicationRun) applicationRoot(documentRoot string) string {
	if !path.IsAbs(documentRoot) {
		return ""
	}
	directory := path.Clean(documentRoot)
	boundary := run.host.userRoot(directory)
	for level := 0; level <= applicationLevelsUp; level++ {
		if strings.Count(directory, "/") < 2 || (boundary != "" && !within(directory, boundary)) {
			return ""
		}
		if run.hasMarker(directory) {
			parent := path.Dir(directory)
			if level < applicationLevelsUp && !run.host.exists(path.Join(directory, "composer.json")) && strings.Count(parent, "/") >= 2 &&
				(boundary == "" || within(parent, boundary)) && run.host.exists(path.Join(parent, "composer.json")) {
				return parent
			}
			return directory
		}
		directory = path.Dir(directory)
	}
	return ""
}

func (run *applicationRun) hasMarker(directory string) bool {
	for _, name := range applicationMarkers {
		if run.host.exists(path.Join(directory, name)) {
			return true
		}
	}
	core, _ := run.wordPressLayout(directory)
	return core != ""
}

// wordPressLayout returns the core and content directories of a WordPress
// install at root, "" when there is none.
func (run *applicationRun) wordPressLayout(root string) (core, content string) {
	for _, layout := range wordPressLayouts {
		candidate := path.Join(root, layout.core)
		if run.host.exists(path.Join(candidate, "wp-includes", "version.php")) {
			return candidate, path.Join(root, layout.content)
		}
	}
	return "", ""
}

// inspect reads the lockfiles, kind, version and WordPress extensions of
// an application.
func (run *applicationRun) inspect(item *Application) {
	root := item.Path
	run.watched = append(run.watched, root)
	manifests := run.manifests(root)
	packages := map[packageKey]*ApplicationPackage{}
	var composerVersions map[string]string
	for _, name := range lockfiles {
		file := path.Join(root, name)
		if !run.host.exists(file) {
			continue
		}
		data, err := run.host.readBoundedFile(file, maxLockfileSize)
		if err != nil {
			run.warn("%s: %v", file, err)
			continue
		}
		parsed, err := parseLockfile(name, data, manifests)
		clear(data)
		if err != nil {
			run.warn("%s: %v", file, err)
			continue
		}
		item.Lockfiles = append(item.Lockfiles, name)
		if len(parsed) > maxLockfilePackages {
			parsed = parsed[:maxLockfilePackages]
			item.Truncated = true
		}
		if name == "composer.lock" {
			composerVersions = map[string]string{}
			for _, entry := range parsed {
				composerVersions[entry.Name] = entry.Version
			}
		}
		for _, entry := range parsed {
			key := packageKey{entry.Ecosystem, entry.Name, entry.Version}
			if existing, ok := packages[key]; ok {
				existing.Direct = existing.Direct || entry.Direct
				existing.Dev = existing.Dev && entry.Dev
				continue
			}
			if !run.reserve(1) {
				item.Truncated = true
				break
			}
			copied := entry
			packages[key] = &copied
		}
	}
	for _, entry := range packages {
		item.Packages = append(item.Packages, *entry)
	}

	if core, content := run.wordPressLayout(root); core != "" {
		item.Kind = ApplicationKindWordPress
		item.Version = run.wordPressVersion(core)
		item.WordPress = run.wordPress(item, content)
		run.watched = append(run.watched, path.Join(core, "wp-includes"))
		return
	}
	switch {
	case run.host.exists(path.Join(root, "artisan")) || composerVersions["laravel/framework"] != "":
		item.Kind = ApplicationKindLaravel
		item.Version = optionalString(strings.TrimPrefix(composerVersions["laravel/framework"], "v"))
	case run.host.exists(path.Join(root, "symfony.lock")) || composerVersions["symfony/framework-bundle"] != "":
		item.Kind = ApplicationKindSymfony
		item.Version = optionalString(strings.TrimPrefix(composerVersions["symfony/http-kernel"], "v"))
	case run.host.exists(path.Join(root, "composer.json")) || composerVersions != nil:
		item.Kind = ApplicationKindComposer
	default:
		item.Kind = ApplicationKindNode
	}
}

// reserve claims room for count entries, false when the server's limit is
// reached.
func (run *applicationRun) reserve(count int) bool {
	if run.entries+count > maxApplicationEntries {
		run.truncated = true
		return false
	}
	run.entries += count
	return true
}

type packageKey struct{ ecosystem, name, version string }

// manifests reads the dependency names of composer.json and package.json,
// nothing else from them.
func (run *applicationRun) manifests(root string) applicationManifests {
	manifests := applicationManifests{}
	for _, name := range []string{"composer.json", "package.json"} {
		file := path.Join(root, name)
		if !run.host.exists(file) {
			continue
		}
		data, err := run.host.readBoundedFile(file, maxLockfileSize)
		if err != nil {
			run.warn("%s: %v", file, err)
			continue
		}
		if name == "composer.json" {
			manifests.composer, err = parseManifest(data, "require", "require-dev")
		} else {
			manifests.npm, err = parseManifest(data, "dependencies", "devDependencies", "optionalDependencies")
		}
		if err != nil {
			run.warn("%s: %v", file, err)
		}
	}
	return manifests
}

var wordPressVersionPattern = regexp.MustCompile(`(?m)^\s*\$wp_version\s*=\s*['"]([^'"]+)['"]`)

// wordPressVersion reads $wp_version from wp-includes/version.php.
func (run *applicationRun) wordPressVersion(core string) *string {
	file := path.Join(core, "wp-includes", "version.php")
	data, err := run.readHeader(file, 64<<10)
	if err != nil {
		run.warn("%s: %v", file, err)
		return nil
	}
	if match := wordPressVersionPattern.FindSubmatch(data); match != nil {
		return optionalString(string(match[1]))
	}
	return nil
}

// wordPress lists the plugins, must-use plugins and themes of a WordPress
// install's content directory.
func (run *applicationRun) wordPress(item *Application, content string) *WordPressInfo {
	info := &WordPressInfo{}
	count := 0
	add := func() bool {
		if count >= maxWordPressExtensions || !run.reserve(1) {
			item.Truncated = true
			return false
		}
		count++
		return true
	}

	plugins := path.Join(content, "plugins")
	run.watched = append(run.watched, plugins)
	entries, _ := run.host.readDir(plugins)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		var header wordPressHeader
		full := path.Join(plugins, name)
		switch {
		case run.host.isDir(full):
			header = run.pluginHeader(full)
		case strings.HasSuffix(name, ".php"):
			header = run.fileHeader(full, "Plugin Name")
			name = strings.TrimSuffix(name, ".php")
		default:
			continue
		}
		if header.name == nil || !add() {
			continue
		}
		info.Plugins = append(info.Plugins, WordPressPlugin{Slug: name, Name: header.name, Version: header.version})
	}

	mustUse := path.Join(content, "mu-plugins")
	run.watched = append(run.watched, mustUse)
	entries, _ = run.host.readDir(mustUse)
	for _, entry := range entries {
		name := entry.Name()
		// WordPress loads every top-level PHP file but index.php, with or
		// without a header.
		if !strings.HasSuffix(name, ".php") || name == "index.php" || strings.HasPrefix(name, ".") || run.host.isDir(path.Join(mustUse, name)) || !add() {
			continue
		}
		header := run.fileHeader(path.Join(mustUse, name), "Plugin Name")
		info.Plugins = append(info.Plugins, WordPressPlugin{Slug: strings.TrimSuffix(name, ".php"), Name: header.name, Version: header.version, MustUse: true})
	}

	themes := path.Join(content, "themes")
	run.watched = append(run.watched, themes)
	entries, _ = run.host.readDir(themes)
	for _, entry := range entries {
		name := entry.Name()
		directory := path.Join(themes, name)
		if strings.HasPrefix(name, ".") || !run.host.isDir(directory) {
			continue
		}
		slug, header := run.themeHeader(directory, name)
		if header.name == nil || !add() {
			continue
		}
		info.Themes = append(info.Themes, WordPressTheme{Slug: slug, Name: header.name, Version: header.version, Parent: header.parent})
	}
	return info
}

// wordPressHeader holds the header fields that are collected.
type wordPressHeader struct {
	name, version, parent *string
}

// pluginHeader returns the header of the first top-level PHP file of a
// plugin directory with a Plugin Name, like get_plugins().
func (run *applicationRun) pluginHeader(directory string) wordPressHeader {
	entries, err := run.host.readDir(directory)
	if err != nil {
		return wordPressHeader{}
	}
	files := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".php") || entry.IsDir() {
			continue
		}
		if files++; files > maxPluginFiles {
			break
		}
		if header := run.fileHeader(path.Join(directory, entry.Name()), "Plugin Name"); header.name != nil {
			return header
		}
	}
	return wordPressHeader{}
}

// themeHeader reads a theme's style.css, one directory deeper when the
// directory itself has none, like search_theme_directories().
func (run *applicationRun) themeHeader(directory, slug string) (string, wordPressHeader) {
	if style := path.Join(directory, "style.css"); run.host.exists(style) {
		return slug, run.fileHeader(style, "Theme Name")
	}
	entries, _ := run.host.readDir(directory)
	for _, entry := range entries {
		style := path.Join(directory, entry.Name(), "style.css")
		if entry.IsDir() && run.host.exists(style) {
			if header := run.fileHeader(style, "Theme Name"); header.name != nil {
				return slug + "/" + entry.Name(), header
			}
		}
	}
	return slug, wordPressHeader{}
}

// fileHeader reads the header fields of a plugin or theme file from its
// first 8 KiB, like get_file_data(). name is nil when the file has no
// nameField.
func (run *applicationRun) fileHeader(file, nameField string) wordPressHeader {
	data, err := run.readHeader(file, wordPressHeaderBytes)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			run.warn("%s: %v", file, err)
		}
		return wordPressHeader{}
	}
	text := strings.ReplaceAll(string(data), "\r", "\n")
	header := wordPressHeader{name: headerField(text, nameField)}
	if header.name == nil {
		return header
	}
	header.version = headerField(text, "Version")
	if nameField == "Theme Name" {
		header.parent = headerField(text, "Template")
	}
	return header
}

var (
	headerCommentEnd = regexp.MustCompile(`\s*(?:\*/|\?>).*`)
	// headerPatterns are get_file_data()'s patterns of the fields read.
	headerPatterns = map[string]*regexp.Regexp{}
)

func init() {
	for _, field := range []string{"Plugin Name", "Theme Name", "Version", "Template"} {
		headerPatterns[field] = regexp.MustCompile(`(?mi)^(?:[ \t]*<\?php)?[ \t/*#@]*` + regexp.QuoteMeta(field) + `:(.*)$`)
	}
}

// headerField returns a WordPress file header field, nil when it is
// missing or empty.
func headerField(text, field string) *string {
	match := headerPatterns[field].FindStringSubmatch(text)
	if match == nil {
		return nil
	}
	value := strings.TrimSpace(headerCommentEnd.ReplaceAllString(match[1], ""))
	if value == "" || len(value) > 200 {
		return nil
	}
	return &value
}

// readHeader reads at most limit bytes from the start of a regular file,
// with readFile's symbolic link policy.
func (run *applicationRun) readHeader(name string, limit int64) ([]byte, error) {
	resolved, info, err := run.host.resolvePath(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errNotRegularFile
	}
	file, err := run.host.FS.Open(relative(resolved))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit))
}
