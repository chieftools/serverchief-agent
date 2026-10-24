package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// rpmRepoKeys are the only .repo and [main] keys the parser keeps. Every
// other value, such as username, password, proxy_password or
// sslclientkey, is dropped while reading and never stored.
var rpmRepoKeys = map[string]bool{
	"name": true, "baseurl": true, "mirrorlist": true, "metalink": true,
	"enabled": true, "gpgcheck": true, "gpgkey": true,
}

// iniSection is one [section] of a .repo file with the kept keys.
type iniSection struct {
	name   string
	values map[string]string
}

// parseRepoINI parses yum/dnf INI files: "[id]" sections, "key=value"
// lines, "#" and ";" comments and indented continuation lines, which
// append to the previous value (multi-line baseurl and gpgkey). Keys not
// in rpmRepoKeys are discarded.
func parseRepoINI(data []byte) []iniSection {
	var sections []iniSection
	var current *iniSection
	last := ""
	for _, line := range lines(data) {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";"):
			// A blank line or comment ends a continuation.
			last = ""
		case strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]"):
			sections = append(sections, iniSection{name: strings.TrimSpace(trimmed[1 : len(trimmed)-1]), values: map[string]string{}})
			current, last = &sections[len(sections)-1], ""
		case current == nil:
		case line[0] == ' ' || line[0] == '\t':
			if last != "" {
				current.values[last] += "\n" + trimmed
			}
		default:
			key, value, found := strings.Cut(trimmed, "=")
			if !found {
				last = ""
				continue
			}
			key = strings.ToLower(strings.TrimSpace(key))
			if !rpmRepoKeys[key] {
				last = ""
				continue
			}
			current.values[key] = strings.TrimSpace(value)
			last = key
		}
	}
	return sections
}

// rpmBool parses dnf's boolean spellings; nil when value is not one.
func rpmBool(value string) *bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "yes", "true", "on":
		return boolPointer(true)
	case "0", "no", "false", "off":
		return boolPointer(false)
	}
	return nil
}

// rpmURIList splits a baseurl or gpgkey value: URLs separated by
// whitespace, commas or lines.
func rpmURIList(value string) []string {
	return strings.FieldsFunc(value, func(character rune) bool {
		return character == ',' || character == ' ' || character == '\t' || character == '\n'
	})
}

// rpmMainGPGCheck returns [main] gpgcheck of dnf.conf or yum.conf, false
// when the file has none (the yum and dnf default) and nil without the
// file.
func (host *Host) rpmMainGPGCheck(manager string) *bool {
	files := []string{"/etc/yum.conf"}
	if manager == ManagerDnf {
		files = []string{"/etc/dnf/dnf.conf", "/etc/yum.conf"}
	}
	for _, file := range files {
		data, ok := host.readSourceFile(file)
		if !ok {
			continue
		}
		for _, section := range parseRepoINI(data) {
			if section.name == "main" {
				if value := rpmBool(section.values["gpgcheck"]); value != nil {
					return value
				}
			}
		}
		return boolPointer(false)
	}
	return nil
}

// rpmRepositories reads the sections of /etc/yum.repos.d/*.repo with
// $releasever and $basearch left unexpanded.
func (host *Host) rpmRepositories(manager string) []Repository {
	mainGPGCheck := host.rpmMainGPGCheck(manager)
	var repositories []Repository
	for _, file := range host.sourceFiles(yumReposDir, ".repo") {
		data, ok := host.readSourceFile(file)
		if !ok {
			continue
		}
		for _, section := range parseRepoINI(data) {
			if section.name == "" || section.name == "main" {
				continue
			}
			repositories = append(repositories, rpmRepository(file, section, mainGPGCheck))
		}
	}
	return repositories
}

func rpmRepository(file string, section iniSection, mainGPGCheck *bool) Repository {
	values := section.values
	repository := Repository{
		ID:      section.name,
		File:    file,
		Type:    RepositoryRPM,
		Enabled: true,
		URIs:    []string{},
		Name:    values["name"],
		Signing: RepositorySigning{GPGCheck: mainGPGCheck},
	}
	if enabled := rpmBool(values["enabled"]); enabled != nil {
		repository.Enabled = *enabled
	}
	if gpgcheck := rpmBool(values["gpgcheck"]); gpgcheck != nil {
		repository.Signing.GPGCheck = gpgcheck
	}
	if keys := rpmURIList(values["gpgkey"]); len(keys) > 0 {
		repository.Signing.SignedBy = stringPointer(sanitizeURI(keys[0]))
	}
	var raw []string
	for _, uri := range rpmURIList(values["baseurl"]) {
		raw = append(raw, uri)
		repository.URIs = append(repository.URIs, sanitizeURI(uri))
	}
	for _, kind := range []string{"mirrorlist", "metalink"} {
		if uri := strings.TrimSpace(values[kind]); uri != "" {
			raw = append(raw, uri)
			repository.URIs = append(repository.URIs, kind+":"+sanitizeMirrorURI(uri))
		}
	}
	if len(raw) > 0 {
		repository.Transport = uriTransport(raw[0])
	}
	return repository
}

// applyRepolist sets the effective enabled state from `dnf repolist --all`
// or `yum repolist all`, cache only. A held lock returns a BusyError; any
// other failure keeps the state of the .repo files.
func (host *Host) applyRepolist(ctx context.Context, manager string, repositories []Repository) error {
	args := []string{"repolist", "--all"}
	if manager == ManagerYum {
		args = []string{"repolist", "all"}
	}
	result, err := host.rpmQuery(ctx, manager, args...)
	if errors.Is(err, ErrPackageManagerBusy) {
		return err
	}
	if err != nil || result.ExitCode != 0 {
		return nil
	}
	states := parseRepolist(result.Stdout)
	for position := range repositories {
		if enabled, found := states[repositories[position].ID]; found {
			repositories[position].Enabled = enabled
		}
	}
	return nil
}

// parseRepolist reads "repo id  repo name  status" columns. yum appends
// "/$releasever/$basearch" to ids, marks expired ones with "!" and prints
// "enabled: <count>".
func parseRepolist(data []byte) map[string]bool {
	states := map[string]bool{}
	for _, line := range lines(data) {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == "repo" && fields[1] == "id" {
			continue
		}
		id, _, _ := strings.Cut(strings.TrimLeft(fields[0], "!*"), "/")
		if id == "" {
			continue
		}
		for index := len(fields) - 1; index >= 1; index-- {
			status := strings.ToLower(fields[index])
			if status == "enabled" || status == "enabled:" {
				states[id] = true
				break
			}
			if status == "disabled" {
				states[id] = false
				break
			}
		}
	}
	return states
}

// rpmFromRepoFormat is the repoquery format of the installed packages'
// origin. dnf 4 ends each package with a newline itself, dnf 5 needs one.
const rpmFromRepoFormat = `%{name}\t%{arch}\t%{from_repo}\n`

// rpmFromRepos returns, per name/arch, the repositories the installed
// packages came from: `dnf repoquery --installed` or, on yum (EL7, Amazon
// Linux 2, which have no from_repo in repoquery without yum-utils), `yum
// list installed`. Both read only the rpm and history databases. A held
// lock returns a BusyError.
func (host *Host) rpmFromRepos(ctx context.Context, manager string) (map[string][]string, error) {
	args := []string{"repoquery", "--installed", "--qf", rpmFromRepoFormat}
	if manager == ManagerYum {
		args = []string{"list", "installed"}
	}
	result, err := host.rpmQuery(ctx, manager, args...)
	if errors.Is(err, ErrPackageManagerBusy) {
		return nil, err
	}
	verb := manager + " " + args[0] + " " + args[1]
	if err != nil {
		return nil, fmt.Errorf("%s: %w", verb, err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("%s exited with status %d: %s", verb, result.ExitCode, rpmFailure(result))
	}
	if manager == ManagerYum {
		return parseYumListInstalled(result.Stdout), nil
	}
	return parseFromRepo(result.Stdout), nil
}

// parseFromRepo parses rpmFromRepoFormat lines. Values that are not
// repository ids ("@System", "@commandline", "anaconda", "<unknown>")
// are kept here and dropped when matched against the configured ids.
func parseFromRepo(data []byte) map[string][]string {
	origins := map[string][]string{}
	for _, line := range lines(data) {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 || parts[0] == "" {
			continue
		}
		key := parts[0] + "/" + parts[1]
		origins[key] = appendUnique(origins[key], strings.TrimSpace(parts[2]))
	}
	return origins
}

// parseYumListInstalled parses `yum list installed`: name.arch, version
// and "@repo" (or "installed" without yumdb data), with long names
// wrapped onto their own line.
func parseYumListInstalled(data []byte) map[string][]string {
	origins := map[string][]string{}
	var fields []string
	for _, line := range lines(data) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "Installed Packages") || strings.HasPrefix(trimmed, "Loaded plugins") ||
			strings.HasPrefix(trimmed, "Loading mirror") {
			fields = nil
			continue
		}
		fields = append(fields, strings.Fields(trimmed)...)
		if len(fields) < 3 {
			continue
		}
		name, arch, ok := cutLast(fields[0], ".")
		if ok && name != "" {
			repository, _, _ := strings.Cut(strings.TrimPrefix(fields[2], "@"), "/")
			origins[name+"/"+arch] = appendUnique(origins[name+"/"+arch], repository)
		}
		fields = nil
	}
	return origins
}
