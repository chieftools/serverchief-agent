package collector

import (
	"context"
	"fmt"
	"strings"
)

const (
	directAdminUsers   = "/usr/local/directadmin/data/users"
	directAdminOptions = "/usr/local/directadmin/custombuild/options.conf"
)

// directAdminModes maps custombuild's php<N>_mode to a handler.
var directAdminModes = map[string]string{
	"php-fpm":  PHPHandlerFPM,
	"fastcgi":  PHPHandlerFastCGI,
	"suphp":    PHPHandlerCGI,
	"lsphp":    PHPHandlerLSAPI,
	"mod_php":  PHPHandlerModule,
	"mod_ruid": PHPHandlerModule,
}

// collectDirectAdmin reads DirectAdmin's own data files:
//
//	data/users/<user>/domains.list               one domain per line
//	data/users/<user>/domains/<domain>.conf      ssl=ON, php=ON, php1_select=2
//	data/users/<user>/domains/<domain>.subdomains one subdomain label per line
//	data/users/<user>/domains/<domain>.pointers   alias.example=type=alias
//	data/users/<user>/domains/<domain>.cert       the public certificate
//	custombuild/options.conf                       php2_release=8.2, php2_mode=php-fpm
//
// Document roots follow DirectAdmin's fixed layout under the user's home
// directory (from /etc/passwd, else /home/<user>).
func collectDirectAdmin(_ context.Context, run *vhostRun) ([]VHost, error) {
	host := run.host
	entries, err := host.readDir(directAdminUsers)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", directAdminUsers, err)
	}
	options := map[string]string{}
	if data, err := host.readFile(directAdminOptions); err == nil {
		options = parseKeyValue(data)
	}
	homes := passwdHomes(host)

	var items []VHost
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		user := entry.Name()
		userDir := directAdminUsers + "/" + user
		data, err := host.readFile(userDir + "/domains.list")
		if err != nil {
			continue
		}
		home := homes[user]
		if home == "" {
			home = "/home/" + user
		}
		for _, line := range lines(data) {
			domain := strings.ToLower(strings.TrimSpace(line))
			if domain == "" || strings.ContainsAny(domain, "/ \t") {
				continue
			}
			items = append(items, directAdminDomain(run, user, userDir, home, domain, options)...)
		}
	}
	return items, nil
}

// directAdminDomain returns the items of one domain and its subdomains.
func directAdminDomain(run *vhostRun, user, userDir, home, domain string, options map[string]string) []VHost {
	host := run.host
	base := userDir + "/domains/" + domain
	configFile := base + ".conf"
	config := map[string]string{}
	if data, err := host.readFile(configFile); err == nil {
		config = parseKeyValue(data)
	}

	owner := user
	item := VHost{
		Name:  domain,
		Ports: []int{80},
		SSL:   strings.EqualFold(config["ssl"], "on"),
		Owner: &owner,
	}
	if host.exists(configFile) {
		item.ConfigFile = &configFile
	}
	root := home + "/domains/" + domain + "/public_html"
	item.DocumentRoot = &root
	if item.SSL {
		item.Ports = append(item.Ports, 443)
		certificate := config["SSLCertificateFile"]
		if certificate == "" && host.exists(base+".cert") {
			certificate = base + ".cert"
		}
		item.Certificate = run.certificate(certificate)
	}
	if !strings.EqualFold(config["php"], "off") {
		item.PHP = directAdminPHP(config, options)
	}
	if data, err := host.readFile(base + ".pointers"); err == nil {
		for _, line := range lines(data) {
			name, rest, _ := strings.Cut(strings.TrimSpace(line), "=")
			// type=pointer is a redirect to the domain; only type=alias
			// serves the same site.
			if name != "" && strings.Contains(rest, "type=alias") {
				item.Aliases = append(item.Aliases, strings.ToLower(name))
			}
		}
	}

	items := []VHost{item}
	if data, err := host.readFile(base + ".subdomains"); err == nil {
		for _, line := range lines(data) {
			label := strings.ToLower(strings.TrimSpace(line))
			if label == "" || strings.ContainsAny(label, "/ \t") {
				continue
			}
			subdomain := item
			subdomain.Name = label + "." + domain
			subdomain.Aliases = nil
			subdomainRoot := root + "/" + label
			subdomain.DocumentRoot = &subdomainRoot
			items = append(items, subdomain)
		}
	}
	return items
}

// directAdminPHP resolves php<N>_select (default 1) through custombuild's
// php<N>_release and php<N>_mode.
func directAdminPHP(config, options map[string]string) *VHostPHP {
	selection := config["php1_select"]
	if selection == "" || selection == "0" {
		selection = "1"
	}
	release := options["php"+selection+"_release"]
	mode := options["php"+selection+"_mode"]
	if release == "no" {
		return nil
	}
	if release == "" && mode == "" {
		return nil
	}
	var version *string
	if release != "" {
		version = phpVersionFrom("php" + release)
	}
	return newPHP(version, directAdminModes[mode])
}

// passwdHomes maps user names to home directories from /etc/passwd.
func passwdHomes(host *Host) map[string]string {
	homes := map[string]string{}
	data, err := host.readFile("/etc/passwd")
	if err != nil {
		return homes
	}
	for _, line := range lines(data) {
		fields := strings.Split(line, ":")
		if len(fields) >= 7 && fields[0] != "" && strings.HasPrefix(fields[5], "/") {
			homes[fields[0]] = fields[5]
		}
	}
	return homes
}
