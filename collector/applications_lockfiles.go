package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// manifestDependencies are the dependency names a manifest requires: in
// production (composer require, npm dependencies and optionalDependencies)
// and for development only.
type manifestDependencies struct {
	production map[string]string
	dev        map[string]string
}

func (manifest *manifestDependencies) requires(name string) bool {
	if manifest == nil {
		return false
	}
	_, production := manifest.production[name]
	_, dev := manifest.dev[name]
	return production || dev
}

// applicationManifests are an application's composer.json and package.json
// dependencies, nil when the file is missing.
type applicationManifests struct {
	composer *manifestDependencies
	npm      *manifestDependencies
}

// parseManifest reads the dependency objects of composer.json or
// package.json. The last field holds the development dependencies.
func parseManifest(data []byte, fields ...string) (*manifestDependencies, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	manifest := &manifestDependencies{production: map[string]string{}, dev: map[string]string{}}
	for index, field := range fields {
		var dependencies map[string]string
		if raw, ok := document[field]; !ok || json.Unmarshal(raw, &dependencies) != nil {
			continue
		}
		target := manifest.production
		if index == len(fields)-1 {
			target = manifest.dev
		}
		for name, constraint := range dependencies {
			target[name] = constraint
		}
	}
	return manifest, nil
}

// parseLockfile returns the packages of a lockfile. A package that only
// development dependencies need is marked dev: from the lockfile's own
// flags where it has them, else by walking the dependency graph from the
// manifest's production dependencies.
func parseLockfile(name string, data []byte, manifests applicationManifests) ([]ApplicationPackage, error) {
	switch name {
	case "composer.lock":
		return parseComposerLock(data, manifests.composer)
	case "package-lock.json", "npm-shrinkwrap.json":
		return parsePackageLock(data, manifests.npm)
	case "yarn.lock":
		return parseYarnLock(data, manifests.npm)
	case "pnpm-lock.yaml":
		return parsePnpmLock(data, manifests.npm)
	}
	return nil, fmt.Errorf("unknown lockfile %s", name)
}

func parseComposerLock(data []byte, manifest *manifestDependencies) ([]ApplicationPackage, error) {
	var lock struct {
		Packages    []composerLockPackage `json:"packages"`
		PackagesDev []composerLockPackage `json:"packages-dev"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, err
	}
	var packages []ApplicationPackage
	for index, list := range [][]composerLockPackage{lock.Packages, lock.PackagesDev} {
		for _, entry := range list {
			if entry.Name == "" || entry.Version == "" {
				continue
			}
			packages = append(packages, ApplicationPackage{
				Ecosystem: EcosystemComposer,
				Name:      strings.ToLower(entry.Name),
				Version:   entry.Version,
				Direct:    manifest.requires(entry.Name),
				Dev:       index == 1,
			})
		}
	}
	return packages, nil
}

type composerLockPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type packageLockEntry struct {
	Name         string                      `json:"name"`
	Version      string                      `json:"version"`
	Dev          bool                        `json:"dev"`
	Link         bool                        `json:"link"`
	Dependencies map[string]packageLockEntry `json:"dependencies"`
}

// parsePackageLock reads npm's lockfile: the packages map of lockfile
// versions 2 and 3, else the nested dependencies of version 1.
func parsePackageLock(data []byte, manifest *manifestDependencies) ([]ApplicationPackage, error) {
	var lock struct {
		Packages     map[string]json.RawMessage  `json:"packages"`
		Dependencies map[string]packageLockEntry `json:"dependencies"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, err
	}
	var packages []ApplicationPackage
	if len(lock.Packages) > 0 {
		if manifest == nil {
			if raw, ok := lock.Packages[""]; ok {
				manifest, _ = parseManifest(raw, "dependencies", "optionalDependencies", "devDependencies")
			}
		}
		for key, raw := range lock.Packages {
			if key == "" {
				continue
			}
			var entry packageLockEntry
			if json.Unmarshal(raw, &entry) != nil || entry.Link {
				continue
			}
			index := strings.LastIndex(key, "node_modules/")
			if index < 0 {
				// A workspace package, not an installed dependency.
				continue
			}
			name := key[index+len("node_modules/"):]
			if entry.Name != "" {
				name = entry.Name
			}
			if !registryVersion(entry.Version) {
				continue
			}
			// Only node_modules/<name> is installed for the project itself.
			topLevel := index == 0
			packages = append(packages, ApplicationPackage{
				Ecosystem: EcosystemNPM,
				Name:      name,
				Version:   entry.Version,
				Direct:    topLevel && manifest.requires(name),
				Dev:       entry.Dev,
			})
		}
		return packages, nil
	}
	var walk func(dependencies map[string]packageLockEntry, topLevel bool)
	walk = func(dependencies map[string]packageLockEntry, topLevel bool) {
		for name, entry := range dependencies {
			if registryVersion(entry.Version) {
				packages = append(packages, ApplicationPackage{
					Ecosystem: EcosystemNPM,
					Name:      name,
					Version:   entry.Version,
					Direct:    topLevel && manifest.requires(name),
					Dev:       entry.Dev,
				})
			}
			walk(entry.Dependencies, false)
		}
	}
	walk(lock.Dependencies, true)
	return packages, nil
}

// registryVersion reports whether an npm version is a registry version
// rather than a link, file, git or tarball reference.
func registryVersion(version string) bool {
	return version != "" && version[0] >= '0' && version[0] <= '9'
}

// lockNode is a package of a lockfile without dev flags, for the
// reachability walk.
type lockNode struct {
	name, version string
	dependencies  []string
}

// markDevelopment marks the packages no production dependency of the
// manifest reaches as dev. resolve maps a dependency reference to node
// keys; byName finds a root whose reference did not resolve. Without a
// manifest nothing is marked, since nothing tells development apart.
func markDevelopment(nodes map[string]*lockNode, roots []string, manifest *manifestDependencies, packages []ApplicationPackage, resolve func(string) []string) []ApplicationPackage {
	reached := map[string]bool{}
	pending := slices.Clone(roots)
	for len(pending) > 0 {
		key := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if reached[key] {
			continue
		}
		reached[key] = true
		if node, ok := nodes[key]; ok {
			for _, dependency := range node.dependencies {
				pending = append(pending, resolve(dependency)...)
			}
		}
	}
	productionVersions := map[packageKey]bool{}
	for key := range reached {
		if node, ok := nodes[key]; ok {
			productionVersions[packageKey{EcosystemNPM, node.name, node.version}] = true
		}
	}
	for index := range packages {
		entry := &packages[index]
		entry.Direct = manifest.requires(entry.Name)
		entry.Dev = manifest != nil && !productionVersions[packageKey{EcosystemNPM, entry.Name, entry.Version}]
	}
	return packages
}

// splitDescriptor splits name@range, keeping a scope's leading @.
func splitDescriptor(descriptor string) (name, rest string) {
	index := strings.LastIndex(descriptor, "@")
	if index <= 0 {
		return descriptor, ""
	}
	// An npm: alias (name@npm:other@range) splits at its first @.
	if first := strings.Index(descriptor[1:], "@") + 1; first > 0 && strings.HasPrefix(descriptor[first+1:], "npm:") {
		return descriptor[:first], descriptor[first+1:]
	}
	return descriptor[:index], descriptor[index+1:]
}

// lockLine is one line of a YAML-like lockfile: its indentation and its
// text without it.
type lockLine struct {
	indent int
	text   string
}

func lockLines(data []byte) []lockLine {
	var result []lockLine
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		text := strings.TrimRight(string(line), " \r")
		trimmed := strings.TrimLeft(text, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		result = append(result, lockLine{indent: len(text) - len(trimmed), text: trimmed})
	}
	return result
}

// splitKeyValue splits `key: value`, `"key": value` or yarn v1's
// `key "value"`, unquoting both.
func splitKeyValue(text string) (key, value string) {
	if strings.HasPrefix(text, `"`) || strings.HasPrefix(text, "'") {
		quote := text[:1]
		if end := strings.Index(text[1:], quote); end >= 0 {
			key, value = text[1:end+1], text[end+2:]
		}
	} else if index := strings.IndexAny(text, ": "); index >= 0 {
		key, value = text[:index], text[index:]
	} else {
		key = text
	}
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), ":"))
	return key, strings.Trim(value, `"'`)
}

// parseYarnLock reads yarn.lock of Yarn 1 and of Yarn 2 and later, whose
// npm: entries are kept. Yarn has no dev flags.
func parseYarnLock(data []byte, manifest *manifestDependencies) ([]ApplicationPackage, error) {
	berry := bytes.Contains(data, []byte("\n__metadata:"))
	nodes := map[string]*lockNode{}
	descriptors := map[string]string{}
	var node *lockNode
	var keys []string
	section := ""
	finish := func() {
		if node == nil || !registryVersion(node.version) || node.name == "" {
			return
		}
		key := node.name + "@" + node.version
		if existing, ok := nodes[key]; ok {
			existing.dependencies = append(existing.dependencies, node.dependencies...)
		} else {
			nodes[key] = node
		}
		for _, descriptor := range keys {
			descriptors[descriptor] = key
		}
	}
	for _, line := range lockLines(data) {
		switch {
		case line.indent == 0:
			finish()
			node, keys, section = nil, nil, ""
			header := strings.TrimSuffix(line.text, ":")
			if header == line.text || strings.HasPrefix(header, "__metadata") {
				continue
			}
			for descriptor := range strings.SplitSeq(strings.ReplaceAll(header, `"`, ""), ",") {
				keys = append(keys, strings.TrimSpace(descriptor))
			}
			name, _ := splitDescriptor(keys[0])
			if !berry {
				if _, rest := splitDescriptor(keys[0]); strings.HasPrefix(rest, "npm:") {
					name, _ = splitDescriptor(strings.TrimPrefix(rest, "npm:"))
				}
			}
			node = &lockNode{name: name}
		case node == nil:
		case line.indent == 2:
			key, value := splitKeyValue(line.text)
			section = ""
			switch key {
			case "version":
				node.version = value
			case "resolution":
				// Only registry packages: workspaces, patches, links and git
				// checkouts have no published versions.
				name, rest := splitDescriptor(value)
				if !strings.HasPrefix(rest, "npm:") {
					node.name = ""
				} else {
					node.name = name
				}
			case "dependencies", "optionalDependencies":
				section = key
			}
		case line.indent == 4 && section != "":
			name, constraint := splitKeyValue(line.text)
			if berry && !strings.Contains(constraint, ":") {
				constraint = "npm:" + constraint
			}
			node.dependencies = append(node.dependencies, name+"@"+constraint)
		}
	}
	finish()

	packages := make([]ApplicationPackage, 0, len(nodes))
	for _, node := range nodes {
		packages = append(packages, ApplicationPackage{Ecosystem: EcosystemNPM, Name: node.name, Version: node.version})
	}
	resolve := func(descriptor string) []string {
		if key, ok := descriptors[descriptor]; ok {
			return []string{key}
		}
		// A reference that does not match a header verbatim: every
		// version of the name.
		name, _ := splitDescriptor(descriptor)
		var keys []string
		for key, node := range nodes {
			if node.name == name {
				keys = append(keys, key)
			}
		}
		return keys
	}
	var roots []string
	if manifest != nil {
		for name, constraint := range manifest.production {
			if berry && !strings.Contains(constraint, ":") {
				constraint = "npm:" + constraint
			}
			roots = append(roots, resolve(name+"@"+constraint)...)
		}
	}
	return markDevelopment(nodes, roots, manifest, packages, resolve), nil
}

// parsePnpmLock reads pnpm-lock.yaml: the packages section of lockfile
// versions 5 to 9, the dev flags of versions 5 and 6, and for version 9 the
// importers and snapshots to tell development dependencies apart.
func parsePnpmLock(data []byte, manifest *manifestDependencies) ([]ApplicationPackage, error) {
	lines := lockLines(data)
	if len(lines) == 0 || !strings.HasPrefix(lines[0].text, "lockfileVersion:") {
		return nil, errors.New("not a pnpm lockfile")
	}
	_, lockVersion := splitKeyValue(lines[0].text)
	slashKeys := strings.HasPrefix(lockVersion, "5")

	nodes := map[string]*lockNode{}
	devFlags := map[string]bool{}
	hasDevFlags := false
	direct := map[string]bool{}
	var roots []string
	top, importer, group, entry, section := "", "", "", "", ""
	for _, line := range lines {
		if line.indent == 0 {
			top, _ = splitKeyValue(line.text)
			importer, group, entry, section = "", "", "", ""
			continue
		}
		key, value := splitKeyValue(line.text)
		switch top {
		case "importers":
			switch line.indent {
			case 2:
				importer = key
			case 4:
				group = key
			case 6:
				entry = key
				if importer == "." {
					direct[key] = true
				}
			case 8:
				// Every workspace package's production dependencies are
				// production dependencies of the application.
				if key == "version" && group != "devDependencies" {
					roots = append(roots, pnpmKey(entry, value, false))
				}
			}
		case "dependencies", "optionalDependencies", "devDependencies":
			// Lockfile versions 5 and 6 list the root project's dependencies
			// at the top level.
			if line.indent == 2 {
				direct[key] = true
				entry = key
				if value != "" && top != "devDependencies" {
					roots = append(roots, pnpmKey(key, value, slashKeys))
				}
			} else if line.indent == 4 && key == "version" && top != "devDependencies" {
				roots = append(roots, pnpmKey(entry, value, slashKeys))
			}
		case "packages", "snapshots":
			switch line.indent {
			case 2:
				entry = strings.TrimPrefix(key, "/")
				section = ""
				if _, ok := nodes[entry]; !ok {
					name, version := pnpmNameVersion(entry, slashKeys)
					nodes[entry] = &lockNode{name: name, version: version}
				}
			case 4:
				section = ""
				switch key {
				case "dev":
					hasDevFlags = true
					devFlags[entry] = value == "true"
				case "dependencies", "optionalDependencies":
					section = key
				}
			case 6:
				if section != "" {
					nodes[entry].dependencies = append(nodes[entry].dependencies, pnpmKey(key, value, slashKeys))
				}
			}
		}
	}

	seen := map[packageKey]int{}
	var packages []ApplicationPackage
	for key, node := range nodes {
		if !registryVersion(node.version) {
			continue
		}
		identity := packageKey{EcosystemNPM, node.name, node.version}
		index, ok := seen[identity]
		if !ok {
			index = len(packages)
			seen[identity] = index
			packages = append(packages, ApplicationPackage{Ecosystem: EcosystemNPM, Name: node.name, Version: node.version, Dev: true})
		}
		packages[index].Dev = packages[index].Dev && devFlags[key]
	}
	if hasDevFlags {
		for index := range packages {
			packages[index].Direct = direct[packages[index].Name]
		}
		return packages, nil
	}
	if manifest == nil && len(direct) > 0 {
		manifest = &manifestDependencies{production: map[string]string{}, dev: map[string]string{}}
		for name := range direct {
			manifest.production[name] = ""
		}
	}
	resolve := func(key string) []string { return []string{key} }
	return markDevelopment(nodes, roots, manifest, packages, resolve), nil
}

// pnpmKey is the packages/snapshots key of a dependency reference: the
// name and the version with its peer suffix.
func pnpmKey(name, version string, slashKeys bool) string {
	if slashKeys {
		return name + "/" + version
	}
	if strings.HasPrefix(version, "/") {
		// An alias resolving to another package.
		return strings.TrimPrefix(version, "/")
	}
	return name + "@" + version
}

// pnpmNameVersion splits a packages key: name@version(peers) or, for
// lockfile version 5, name/version_peers.
func pnpmNameVersion(key string, slashKeys bool) (string, string) {
	if slashKeys {
		index := strings.LastIndex(key, "/")
		if index <= 0 {
			return key, ""
		}
		version, _, _ := strings.Cut(key[index+1:], "_")
		return key[:index], version
	}
	base, _, _ := strings.Cut(key, "(")
	name, version := splitDescriptor(base)
	return name, version
}
