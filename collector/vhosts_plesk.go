package collector

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

const (
	pleskRoot = "/usr/local/psa"
	// pleskVhostsSystem holds the web server configuration Plesk generates
	// per domain (system/<domain>/conf/httpd.conf).
	pleskVhostsSystem = "/var/www/vhosts/system"
)

// pleskColumnsQuery lists the columns the domains query may use, so a
// Plesk release without one of them still works.
const pleskColumnsQuery = "SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.COLUMNS" +
	" WHERE TABLE_SCHEMA = 'psa' AND TABLE_NAME IN ('domains', 'hosting', 'domain_aliases', 'certificates')"

// pleskColumns maps table to its column names.
type pleskColumns map[string]map[string]bool

func (columns pleskColumns) has(table, column string) bool {
	return columns[table][column]
}

// pleskQuery runs a read-only query against the psa database with
// `plesk db -Ne`, which prints tab-separated rows without a header.
func (host *Host) pleskQuery(ctx context.Context, query string) ([][]string, error) {
	result, err := host.run(ctx, host.findCommand("plesk"), "db", "-Ne", query)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("plesk db exited with status %d: %s", result.ExitCode, firstLine(result.Stderr))
	}
	var rows [][]string
	for _, line := range lines(result.Stdout) {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		for index, field := range fields {
			fields[index] = unescapeMySQL(field)
		}
		rows = append(rows, fields)
	}
	return rows, nil
}

// unescapeMySQL reverses the mysql client's batch-mode escaping (\n, \t,
// \\, \0). The literal NULL becomes "".
func unescapeMySQL(field string) string {
	if field == "NULL" {
		return ""
	}
	if !strings.Contains(field, `\`) {
		return field
	}
	var builder strings.Builder
	for index := 0; index < len(field); index++ {
		if field[index] != '\\' || index+1 == len(field) {
			builder.WriteByte(field[index])
			continue
		}
		index++
		switch field[index] {
		case 'n':
			builder.WriteByte('\n')
		case 't':
			builder.WriteByte('\t')
		case '0':
			builder.WriteByte(0)
		default:
			builder.WriteByte(field[index])
		}
	}
	return builder.String()
}

// pleskDomainsQuery builds the domains query from the available columns.
// Missing optional columns read as NULL. Only the public certificate
// (certificates.cert, hex encoded so any line breaks survive the
// tab-separated output) is selected, never the private key, and never
// certificates.cert_file, whose file holds the key as well.
func pleskDomainsQuery(columns pleskColumns) (string, error) {
	for _, required := range [][2]string{{"domains", "id"}, {"domains", "name"}, {"hosting", "dom_id"}} {
		if !columns.has(required[0], required[1]) {
			return "", fmt.Errorf("psa.%s has no %s column", required[0], required[1])
		}
	}
	column := func(table, name, expression string) string {
		if columns.has(table, name) {
			return expression
		}
		return "NULL"
	}
	owner := "d.name"
	join := ""
	if columns.has("domains", "webspace_id") {
		owner = "COALESCE(w.name, d.name)"
		join += " LEFT JOIN domains w ON w.id = d.webspace_id AND d.webspace_id > 0"
	}
	certificate, certificateName, certificateChain := "NULL", "NULL", "NULL"
	if columns.has("hosting", "certificate_id") && columns.has("certificates", "id") && columns.has("certificates", "cert") {
		certificate = "HEX(c.cert)"
		join += " LEFT JOIN certificates c ON c.id = h.certificate_id"
		// The name tells Let's Encrypt certificates apart ("Lets Encrypt
		// example.com"); ca_cert is the public CA chain.
		certificateName = column("certificates", "name", "c.name")
		certificateChain = column("certificates", "ca_cert", "HEX(c.ca_cert)")
	}
	fields := []string{
		"d.id",
		"d.name",
		owner,
		column("hosting", "www_root", "h.www_root"),
		column("hosting", "php", "h.php"),
		column("hosting", "php_handler_id", "h.php_handler_id"),
		column("hosting", "ssl", "h.ssl"),
		certificate,
		certificateName,
		certificateChain,
	}
	return "SELECT " + strings.Join(fields, ", ") + " FROM domains d JOIN hosting h ON h.dom_id = d.id" + join + " ORDER BY d.id", nil
}

// pleskAliasesQuery lists web aliases by domain ID.
func pleskAliasesQuery(columns pleskColumns) string {
	if !columns.has("domain_aliases", "dom_id") || !columns.has("domain_aliases", "name") {
		return ""
	}
	query := "SELECT dom_id, name FROM domain_aliases"
	if columns.has("domain_aliases", "web") {
		query += " WHERE web = 'true'"
	}
	return query + " ORDER BY dom_id, name"
}

func collectPlesk(ctx context.Context, run *vhostRun) ([]VHost, error) {
	items, err := pleskFromDatabase(ctx, run)
	if err == nil {
		return items, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	fallback, fallbackErr := pleskFromSiteList(ctx, run.host)
	if fallbackErr != nil {
		return nil, fmt.Errorf("plesk db: %v; plesk bin site --list: %v", err, fallbackErr)
	}
	return fallback, nil
}

func pleskFromDatabase(ctx context.Context, run *vhostRun) ([]VHost, error) {
	host := run.host
	rows, err := host.pleskQuery(ctx, pleskColumnsQuery)
	if err != nil {
		return nil, err
	}
	columns := pleskColumns{}
	for _, row := range rows {
		if len(row) != 2 {
			continue
		}
		table, column := strings.ToLower(row[0]), strings.ToLower(row[1])
		if columns[table] == nil {
			columns[table] = map[string]bool{}
		}
		columns[table][column] = true
	}
	query, err := pleskDomainsQuery(columns)
	if err != nil {
		return nil, err
	}
	rows, err = host.pleskQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	aliases := map[string][]string{}
	if aliasQuery := pleskAliasesQuery(columns); aliasQuery != "" {
		aliasRows, err := host.pleskQuery(ctx, aliasQuery)
		if err != nil {
			return nil, err
		}
		for _, row := range aliasRows {
			if len(row) == 2 && row[1] != "" {
				aliases[row[0]] = append(aliases[row[0]], strings.ToLower(row[1]))
			}
		}
	}

	items := make([]VHost, 0, len(rows))
	for _, row := range rows {
		if len(row) != 10 || row[1] == "" {
			continue
		}
		id, name, owner, root, php, handler, ssl, certificate := row[0], strings.ToLower(row[1]), strings.ToLower(row[2]), row[3], row[4], row[5], row[6], row[7]
		certificateName, certificateChain := row[8], row[9]
		item := VHost{
			Name:         name,
			Aliases:      aliases[id],
			Ports:        []int{80},
			SSL:          ssl == "true",
			DocumentRoot: optionalString(strings.TrimSuffix(root, "/")),
			Owner:        optionalString(owner),
		}
		if item.SSL {
			item.Ports = append(item.Ports, 443)
		}
		if php != "false" {
			item.PHP = pleskPHP(handler)
		}
		item.Certificate = pleskCertificate(certificate, certificateChain, certificateName)
		if config := pleskVhostsSystem + "/" + name + "/conf/httpd.conf"; host.exists(config) {
			item.ConfigFile = &config
		}
		items = append(items, item)
	}
	return addWWWAliases(items), nil
}

// pleskCertificate parses the certificates.cert value of the domains
// query, with the certificate's ca_cert chain and name. Plesk stores the
// public PEM form-encoded, with "+" for spaces and "%0A" for line breaks:
//
//	-----BEGIN+CERTIFICATE-----%0AMIIF…%0A-----END+CERTIFICATE-----%0A
//
// The query selects it as HEX(), but a plain value (from a query without
// HEX, or a release that stores the PEM as is) is accepted too.
// Certificates named "Lets Encrypt <domain>" are renewed by Plesk's Let's
// Encrypt extension (SSL It!).
func pleskCertificate(value, chain, name string) *VHostCertificate {
	certificates := parseCertificateBlocks(scanCertificatePEM(pleskPEM(value)))
	if len(certificates) == 0 {
		return nil
	}
	certificate := vhostCertificate(certificates[0])
	certificate.source = &certificateSource{
		inline:      certificates,
		inlineChain: parseCertificateBlocks(scanCertificatePEM(pleskPEM(chain))),
	}
	if strings.HasPrefix(name, pleskLetsEncryptPrefix) {
		certificate.source.managed = &CertificateManager{
			Tool:    optionalString(CertificateToolPlesk),
			Name:    optionalString(name),
			Renewal: optionalString(CertificateRenewalPanel),
		}
	}
	return certificate
}

// pleskLetsEncryptPrefix starts the names of the certificates Plesk's Let's
// Encrypt extension issues.
const pleskLetsEncryptPrefix = "Lets Encrypt "

// pleskPEM decodes a hex and/or form-encoded PEM value from psa, or
// returns nil.
func pleskPEM(value string) []byte {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if !strings.HasPrefix(value, "-----") {
		decoded, err := hex.DecodeString(value)
		if err != nil {
			return nil
		}
		value = strings.TrimSpace(string(decoded))
	}
	// Base64 uses "+", so only a form-encoded value may be unescaped: its
	// armor has "+" or "%20" for the space, and "%2B" for every "+".
	if strings.HasPrefix(value, "-----BEGIN+") || strings.HasPrefix(value, "-----BEGIN%20") {
		unescaped, err := url.QueryUnescape(value)
		if err != nil {
			return nil
		}
		value = unescaped
	}
	return []byte(value)
}

// pleskPHP maps a php_handler_id to a version and handler:
// plesk-php82-fpm, plesk-php74-fastcgi, plesk-php56-cgi, or the OS
// vendor's fpm, fastcgi, cgi and module (whose version is not in the ID).
func pleskPHP(id string) *VHostPHP {
	if id == "" {
		return nil
	}
	lower := strings.ToLower(id)
	handler := ""
	switch {
	case strings.Contains(lower, "fpm"):
		handler = PHPHandlerFPM
	case strings.Contains(lower, "fastcgi"):
		handler = PHPHandlerFastCGI
	case strings.Contains(lower, "lsphp") || strings.Contains(lower, "lsapi"):
		handler = PHPHandlerLSAPI
	case strings.Contains(lower, "cgi"):
		handler = PHPHandlerCGI
	case strings.Contains(lower, "module"):
		handler = PHPHandlerModule
	}
	return newPHP(phpVersionFrom(lower), handler)
}

// pleskFromSiteList is the fallback when the database cannot be queried:
// names only.
func pleskFromSiteList(ctx context.Context, host *Host) ([]VHost, error) {
	result, err := host.run(ctx, host.findCommand("plesk"), "bin", "site", "--list")
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("exited with status %d: %s", result.ExitCode, firstLine(result.Stderr))
	}
	var items []VHost
	for _, line := range lines(result.Stdout) {
		name := strings.ToLower(strings.TrimSpace(line))
		if name == "" || strings.ContainsAny(name, " \t") {
			continue
		}
		items = append(items, VHost{Name: name})
	}
	return addWWWAliases(items), nil
}
