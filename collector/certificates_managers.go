package collector

import (
	"encoding/base64"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	certbotRenewalDir = "/etc/letsencrypt/renewal"
	// maxRenewalConfigs bounds the certbot lineages and acme.sh domains
	// read per collection.
	maxRenewalConfigs = 2000
	// maxRenewalConfigFile bounds one renewal configuration or crontab.
	maxRenewalConfigFile = 256 << 10
	// maxCronFiles bounds the files read from /etc/cron.d.
	maxCronFiles = 256
)

// certbotTimers are the systemd timers that run certbot renew: the Debian
// and Ubuntu package's, the snap's and EPEL's.
var certbotTimers = []string{"certbot.timer", "snap.certbot.renew.timer", "certbot-renew.timer"}

// systemdUnitDirectories hold unit files and their *.wants directories.
var systemdUnitDirectories = []string{"/etc/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"}

// rootCrontabs are root's own crontab (Debian, EL); /etc/crontab and
// /etc/cron.d/* are read as well.
var rootCrontabs = []string{"/var/spool/cron/crontabs/root", "/var/spool/cron/root"}

var (
	// certbotRenewCron matches "certbot renew", the certbot-auto and
	// letsencrypt-auto wrappers, and a scheduled "certonly", which renews
	// a lineage when it is due (or always, with --renew-by-default).
	certbotRenewCron = regexp.MustCompile(`(^|[\s/;&|'"])(certbot|letsencrypt)(-auto)?(\s.*)?\s(renew|certonly)(\s|$)`)
	acmeShCron       = regexp.MustCompile(`acme\.sh(\s.*)?\s--cron(\s|$)`)
	// forgeRenewCron matches the job Laravel Forge installs for each Let's
	// Encrypt certificate in /etc/cron.d/letsencrypt-renew-domain-<id>:
	// "43 7 * * 2 root /bin/bash /root/.letsencrypt-renew/300002".
	forgeRenewCron = regexp.MustCompile(`(/\S*/\.letsencrypt-renew/([0-9]+))(\s|$)`)
)

// forgeCertificateGlobs are where Forge keeps the certificate of an ID:
// ssl/domains/<domain>/<id>/server.crt, and ssl/<site>/<id>/server.crt
// before it grouped them by domain.
var forgeCertificateGlobs = []string{"/etc/nginx/ssl/domains/*/%s/server.crt", "/etc/nginx/ssl/*/%s/server.crt"}

// managers returns the certbot and acme.sh managers by the fingerprint of
// the certificate they renew.
func (run *certificateRun) managers() map[string][]*CertificateManager {
	managers := map[string][]*CertificateManager{}
	add := func(certificatePaths []string, manager *CertificateManager) {
		for _, name := range certificatePaths {
			if name == "" || !path.IsAbs(name) {
				continue
			}
			if leaf := run.load(name).leaf(); leaf != nil {
				fingerprint := fingerprintOf(leaf)
				managers[fingerprint] = append(managers[fingerprint], manager)
				return
			}
		}
	}
	for _, lineage := range run.certbotLineages() {
		add(lineage.certificates, lineage.manager)
	}
	for _, domain := range run.acmeShDomains() {
		add(domain.certificates, domain.manager)
	}
	for _, certificate := range run.forgeCertificates() {
		add(certificate.certificates, certificate.manager)
	}
	return managers
}

// renewalConfig is one certbot lineage or acme.sh domain: its manager and
// the certificate files to match it by, in order of preference.
type renewalConfig struct {
	manager      *CertificateManager
	certificates []string
}

// certbotLineages reads /etc/letsencrypt/renewal/*.conf.
func (run *certificateRun) certbotLineages() []renewalConfig {
	entries, err := run.host.readDir(certbotRenewalDir)
	if err != nil {
		return nil
	}
	var result []renewalConfig
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".conf") || entry.IsDir() {
			continue
		}
		if len(result) >= maxRenewalConfigs {
			run.warn("certbot: more than " + strconv.Itoa(maxRenewalConfigs) + " renewal configurations, the rest skipped")
			break
		}
		config := certbotRenewalDir + "/" + name
		data, err := run.host.readBoundedFile(config, maxRenewalConfigFile)
		if err != nil {
			if warning := certificateWarning(config, err); warning != "" {
				run.warn(warning)
			}
			continue
		}
		sections := parseINI(data)
		top, params := sections[""], sections["renewalparams"]
		manager := &CertificateManager{
			Tool:      optionalString(CertificateToolCertbot),
			Name:      optionalString(strings.TrimSuffix(name, ".conf")),
			Plugin:    optionalString(certbotPlugin(params["authenticator"])),
			Installer: optionalString(certbotPlugin(params["installer"])),
			CAURL:     optionalString(params["server"]),
			KeyType:   optionalString(params["key_type"]),
			Renewal:   optionalString(run.certbotRenewalMode()),
			Config:    optionalString(config),
		}
		result = append(result, renewalConfig{manager: manager, certificates: []string{top["cert"], top["fullchain"]}})
	}
	return result
}

// certbotPlugin normalises a plugin name: "None" is no plugin, and the
// legacy third-party form "certbot-dns-example:dns-example" is reduced to
// the plugin's own name.
func certbotPlugin(value string) string {
	if strings.EqualFold(value, "none") {
		return ""
	}
	if _, name, ok := strings.Cut(value, ":"); ok {
		return name
	}
	return value
}

// parseINI parses certbot's configobj files: key = value lines, grouped by
// [section] (top-level keys are in section "").
func parseINI(data []byte) map[string]map[string]string {
	sections := map[string]map[string]string{"": {}}
	current := ""
	for _, line := range lines(data) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.ToLower(strings.TrimSpace(strings.Trim(line, "[]")))
			if sections[current] == nil {
				sections[current] = map[string]string{}
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		sections[current][strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
	}
	return sections
}

// certbotRenewalMode is timer when a certbot timer is enabled, cron when a
// crontab runs certbot renew, and none otherwise.
func (run *certificateRun) certbotRenewalMode() string {
	if run.certbotRenewal == nil {
		mode := CertificateRenewalNone
		switch {
		case run.timerEnabled(certbotTimers...):
			mode = CertificateRenewalTimer
		case run.cronRuns(certbotRenewCron):
			mode = CertificateRenewalCron
		}
		run.certbotRenewal = &mode
	}
	return *run.certbotRenewal
}

// timerEnabled reports whether one of the units is enabled: linked from a
// *.wants directory, and not masked.
func (run *certificateRun) timerEnabled(units ...string) bool {
	host := run.host
	for _, unit := range units {
		if target, err := host.readLink("/etc/systemd/system/" + unit); err == nil && target == "/dev/null" {
			continue
		}
		for _, directory := range systemdUnitDirectories {
			matches, _ := fs.Glob(host.FS, relative(directory+"/*.wants/"+unit))
			if len(matches) > 0 {
				return true
			}
		}
	}
	return false
}

// cronPeriodicDirectories hold the scripts run-parts (or anacron) runs
// from /etc/crontab: a monthly "certbot-auto renew" script is renewal too.
var cronPeriodicDirectories = []string{"/etc/cron.hourly", "/etc/cron.daily", "/etc/cron.weekly", "/etc/cron.monthly"}

// crontabLines returns the active lines of root's crontabs, /etc/crontab,
// /etc/cron.d/* and the executable scripts of cronPeriodicDirectories,
// read once per collection.
func (run *certificateRun) crontabLines() []string {
	if run.crontabsRead {
		return run.crontabs
	}
	run.crontabsRead = true
	host := run.host
	files := append([]string{"/etc/crontab"}, rootCrontabs...)
	// cron and run-parts skip names with dots and editor backups, and
	// run-parts files that are not executable.
	add := func(directory string, executable bool) {
		entries, err := host.readDir(directory)
		if err != nil {
			return
		}
		for index, entry := range entries {
			if index >= maxCronFiles {
				break
			}
			name := entry.Name()
			if entry.IsDir() || strings.ContainsAny(name, ".~") {
				continue
			}
			if info, err := entry.Info(); executable && (err != nil || info.Mode()&0o111 == 0) {
				continue
			}
			files = append(files, directory+"/"+name)
		}
	}
	add("/etc/cron.d", false)
	for _, directory := range cronPeriodicDirectories {
		add(directory, true)
	}
	// Debian's /etc/cron.d/certbot only runs without systemd; on systemd
	// hosts the timer does the work.
	systemd := host.isDir("/run/systemd/system")
	for _, name := range files {
		data, err := host.readBoundedFile(name, maxRenewalConfigFile)
		if err != nil {
			continue
		}
		for _, line := range lines(data) {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if systemd && strings.Contains(line, "-d /run/systemd/system") {
				continue
			}
			run.crontabs = append(run.crontabs, line)
		}
	}
	return run.crontabs
}

func (run *certificateRun) cronRuns(pattern *regexp.Regexp) bool {
	return slices.ContainsFunc(run.crontabLines(), pattern.MatchString)
}

// acmeShHome is root's ~/.acme.sh (ProtectHome=read-only still allows
// reading it).
func acmeShHome(host *Host) string {
	home := passwdHomes(host)["root"]
	if home == "" {
		home = "/root"
	}
	return strings.TrimSuffix(home, "/") + "/.acme.sh"
}

// acmeShServers maps acme.sh's short CA names to directory URLs.
var acmeShServers = map[string]string{
	"letsencrypt":      "https://acme-v02.api.letsencrypt.org/directory",
	"letsencrypt_test": "https://acme-staging-v02.api.letsencrypt.org/directory",
	"zerossl":          "https://acme.zerossl.com/v2/DV90",
	"buypass":          "https://api.buypass.com/acme/directory",
	"buypass_test":     "https://api.test4.buypass.no/acme/directory",
	"google":           "https://dv.acme-v02.api.pki.goog/directory",
	"googletest":       "https://dv.acme-v02.test-api.pki.goog/directory",
	"sslcom":           "https://acme.ssl.com/sslcom-dv-rsa",
}

// acmeShDomains reads ~root/.acme.sh/<domain>[_ecc]/<domain>.conf. Only
// the .conf and the public .cer files are read; the keys next to them and
// the account keys under ca/ are never opened.
func (run *certificateRun) acmeShDomains() []renewalConfig {
	host := run.host
	home := acmeShHome(host)
	entries, err := host.readDir(home)
	if err != nil {
		return nil
	}
	defaults := map[string]string{}
	if data, err := host.readBoundedFile(home+"/account.conf", maxRenewalConfigFile); err == nil {
		defaults = parseAcmeShConf(data)
	}
	defaultServer := defaults["DEFAULT_ACME_SERVER"]
	if defaultServer == "" {
		defaultServer = defaults["ACME_DIRECTORY"]
	}
	var result []renewalConfig
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "ca" || entry.Name() == "deploy" || entry.Name() == "dnsapi" || entry.Name() == "notify" {
			continue
		}
		if len(result) >= maxRenewalConfigs {
			run.warn("acme.sh: more than " + strconv.Itoa(maxRenewalConfigs) + " domains, the rest skipped")
			break
		}
		directory := home + "/" + entry.Name()
		domain := strings.TrimSuffix(entry.Name(), "_ecc")
		config := directory + "/" + domain + ".conf"
		data, err := host.readBoundedFile(config, maxRenewalConfigFile)
		if err != nil {
			if warning := certificateWarning(config, err); warning != "" {
				run.warn(warning)
			}
			continue
		}
		values := parseAcmeShConf(data)
		server := values["Le_API"]
		if server == "" {
			server = defaultServer
		}
		if known, ok := acmeShServers[strings.ToLower(server)]; ok {
			server = known
		}
		name := values["Le_Domain"]
		if name == "" {
			name = domain
		}
		manager := &CertificateManager{
			Tool:    optionalString(CertificateToolAcmeSh),
			Name:    optionalString(name),
			Plugin:  optionalString(acmeShPlugin(values["Le_Webroot"])),
			CAURL:   optionalString(server),
			KeyType: optionalString(values["Le_Keylength"]),
			Renewal: optionalString(run.acmeShRenewalMode()),
			Config:  optionalString(config),
		}
		result = append(result, renewalConfig{manager: manager, certificates: []string{
			directory + "/" + domain + ".cer",
			values["Le_RealCertPath"],
			values["Le_RealFullChainPath"],
		}})
	}
	return result
}

// acmeShPlugin maps Le_Webroot: a dns_* API as is, a path to webroot, "no"
// to standalone, and modes such as apache, nginx or alpn as is.
func acmeShPlugin(webroot string) string {
	switch {
	case webroot == "":
		return ""
	case strings.HasPrefix(webroot, "/"):
		return "webroot"
	case webroot == "no":
		return "standalone"
	}
	// Several DNS APIs or webroots are comma separated; the first decides.
	first, _, _ := strings.Cut(webroot, ",")
	if strings.HasPrefix(first, "/") {
		return "webroot"
	}
	return first
}

// parseAcmeShConf parses acme.sh's KEY='value' files, decoding the values
// it stores base64-encoded.
func parseAcmeShConf(data []byte) map[string]string {
	values := parseKeyValue(data)
	for key, value := range values {
		if encoded, ok := strings.CutPrefix(value, "__ACME_BASE64__START_"); ok {
			encoded = strings.TrimSuffix(encoded, "__ACME_BASE64__END_")
			if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil {
				values[key] = string(decoded)
			}
		}
	}
	return values
}

// acmeShRenewalMode is cron when a crontab runs acme.sh --cron, none
// otherwise.
func (run *certificateRun) acmeShRenewalMode() string {
	if run.acmeShRenewal == nil {
		mode := CertificateRenewalNone
		if run.cronRuns(acmeShCron) {
			mode = CertificateRenewalCron
		}
		run.acmeShRenewal = &mode
	}
	return *run.acmeShRenewal
}

// forgeCertificates returns the certificates Laravel Forge renews, one per
// renewal job in the crontabs. The job's script under /root is not read:
// its certificate ID names the certificate's directory.
func (run *certificateRun) forgeCertificates() []renewalConfig {
	seen := map[string]bool{}
	var result []renewalConfig
	for _, line := range run.crontabLines() {
		match := forgeRenewCron.FindStringSubmatch(line)
		if match == nil || seen[match[2]] {
			continue
		}
		if len(result) >= maxRenewalConfigs {
			run.warn("forge: more than " + strconv.Itoa(maxRenewalConfigs) + " renewal jobs, the rest skipped")
			break
		}
		script, id := match[1], match[2]
		seen[id] = true
		var certificates []string
		for _, pattern := range forgeCertificateGlobs {
			matches, _ := fs.Glob(run.host.FS, relative(fmt.Sprintf(pattern, id)))
			for _, name := range matches {
				certificates = append(certificates, "/"+name)
			}
		}
		result = append(result, renewalConfig{
			manager: &CertificateManager{
				Tool:    optionalString(CertificateToolForge),
				Name:    optionalString(id),
				Renewal: optionalString(CertificateRenewalCron),
				Config:  optionalString(script),
			},
			certificates: certificates,
		})
	}
	return result
}
