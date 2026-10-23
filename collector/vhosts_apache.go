package collector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// apacheControls are tried in order until one answers -S. apache2ctl
// sources /etc/apache2/envvars on Debian and Ubuntu; EL's apachectl is a
// systemctl wrapper that rejects -S, so httpd comes before it.
var apacheControls = []string{"apache2ctl", "httpd", "apachectl"}

const (
	// apacheIncludeDepth and apacheIncludeFiles bound how far Include and
	// IncludeOptional are followed inside one virtual host block.
	apacheIncludeDepth = 3
	apacheIncludeFiles = 32
	// maxApacheConfigFile bounds the size of one parsed configuration file.
	maxApacheConfigFile = 4 << 20
)

func (host *Host) apacheControl() string {
	if controls := host.apacheControls(); len(controls) > 0 {
		return controls[0]
	}
	return ""
}

func (host *Host) apacheControls() []string {
	var found []string
	for _, name := range apacheControls {
		if command := host.findCommand(name); command != "" {
			found = append(found, command)
		}
	}
	return found
}

// apacheDumpVHosts runs -S with the first control program that succeeds.
func (host *Host) apacheDumpVHosts(ctx context.Context) ([]byte, error) {
	var failures []string
	for _, control := range host.apacheControls() {
		result, err := host.run(ctx, control, "-S")
		switch {
		case err != nil:
			failures = append(failures, fmt.Sprintf("%s -S: %v", path.Base(control), err))
		case result.ExitCode != 0:
			failures = append(failures, fmt.Sprintf("%s -S exited with status %d: %s", path.Base(control), result.ExitCode, firstLine(append(result.Stderr, result.Stdout...))))
		default:
			// The vhost list goes to stdout; some builds print it to stderr.
			return append(append([]byte{}, result.Stdout...), result.Stderr...), nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.New(strings.Join(failures, "; "))
}

// apacheVHostRef is one "namevhost" entry of apachectl -S.
type apacheVHostRef struct {
	port    int
	name    string
	aliases []string
	file    string
	line    int
}

// apacheDump is the parsed output of apachectl -S.
type apacheDump struct {
	serverRoot string
	vhosts     []apacheVHostRef
}

var (
	apacheNameVHost    = regexp.MustCompile(`^(\S+)\s+is a NameVirtualHost$`)
	apachePortVHost    = regexp.MustCompile(`^port (\d+) namevhost (\S+) \((.+):(\d+)\)$`)
	apacheSingleVHost  = regexp.MustCompile(`^(\S+:\d+)\s+(\S+) \((.+):(\d+)\)$`)
	apacheAlias        = regexp.MustCompile(`^(?:wild )?alias (\S+)$`)
	apacheServerRootRe = regexp.MustCompile(`^ServerRoot: "(.*)"$`)
)

// parseApacheDump parses the VirtualHost section of apachectl -S:
//
//	*:443                  is a NameVirtualHost
//	         default server example.com (/etc/apache2/sites-enabled/example.conf:20)
//	         port 443 namevhost example.com (/etc/apache2/sites-enabled/example.conf:20)
//	                 alias www.example.com
//	*:80                   www.example.org (/etc/httpd/conf.d/example.org.conf:1)
//	ServerRoot: "/etc/apache2"
//
// "default server" lines repeat a namevhost line and are skipped.
func parseApacheDump(data []byte) apacheDump {
	var dump apacheDump
	var current *apacheVHostRef
	for _, raw := range lines(data) {
		line := strings.TrimSpace(raw)
		if match := apacheServerRootRe.FindStringSubmatch(line); match != nil {
			dump.serverRoot = match[1]
			continue
		}
		if match := apacheAlias.FindStringSubmatch(line); match != nil && current != nil {
			current.aliases = append(current.aliases, lowerHostname(match[1]))
			continue
		}
		if apacheNameVHost.MatchString(line) || strings.HasPrefix(line, "default server ") {
			current = nil
			continue
		}
		var port, name, file, number string
		if match := apachePortVHost.FindStringSubmatch(line); match != nil {
			port, name, file, number = match[1], match[2], match[3], match[4]
		} else if match := apacheSingleVHost.FindStringSubmatch(line); match != nil {
			port, name, file, number = match[1][strings.LastIndexByte(match[1], ':')+1:], match[2], match[3], match[4]
		} else {
			current = nil
			continue
		}
		// Without a ServerName, Apache names a vhost after the host's
		// address; that is not a configured name and changes with the
		// address, so such (default) vhosts are left out.
		if _, err := netip.ParseAddr(strings.Trim(name, "[]")); err == nil {
			current = nil
			continue
		}
		portNumber, _ := strconv.Atoi(port)
		lineNumber, _ := strconv.Atoi(number)
		dump.vhosts = append(dump.vhosts, apacheVHostRef{port: portNumber, name: lowerHostname(name), file: file, line: lineNumber})
		current = &dump.vhosts[len(dump.vhosts)-1]
	}
	return dump
}

func collectApache(ctx context.Context, run *vhostRun) ([]VHost, error) {
	host := run.host
	output, err := host.apacheDumpVHosts(ctx)
	if err != nil {
		return nil, err
	}
	dump := parseApacheDump(output)
	if dump.serverRoot == "" {
		dump.serverRoot = "/etc/httpd"
		if host.isDir("/etc/apache2") {
			dump.serverRoot = "/etc/apache2"
		}
	}

	parser := &apacheParser{host: host, serverRoot: dump.serverRoot, files: make(map[string][]string)}
	global := parser.globalPHP()
	blocks := make(map[string]apacheBlock)
	items := make([]VHost, 0, len(dump.vhosts))
	for _, ref := range dump.vhosts {
		location := fmt.Sprintf("%s:%d", ref.file, ref.line)
		block, parsed := blocks[location]
		if !parsed {
			block = parser.block(ref.file, ref.line)
			blocks[location] = block
		}
		item := VHost{
			Name:       ref.name,
			Aliases:    append(slices.Clone(ref.aliases), block.aliases...),
			Ports:      []int{ref.port},
			SSL:        block.ssl || (!block.found && ref.port == 443),
			ConfigFile: optionalString(ref.file),
		}
		if block.documentRoot != "" {
			item.DocumentRoot = optionalString(block.documentRoot)
		}
		item.PHP = block.php
		if item.SSL && block.certificate != "" {
			item.Certificate = run.certificate(absoluteFrom(dump.serverRoot, block.certificate))
		}
		items = append(items, item)
	}
	// The global handler only applies to names without their own, in any
	// of their blocks, so it is filled in after merging them.
	items = mergeVHosts(VHostSourceApache, items)
	module := parser.moduleVersion()
	for index := range items {
		item := &items[index]
		if item.PHP == nil && global != nil {
			php := *global
			item.PHP = &php
		}
		if item.PHP != nil && item.PHP.Version == nil && item.PHP.Handler != nil && *item.PHP.Handler == PHPHandlerModule {
			item.PHP.Version = module
		}
	}
	return items, nil
}

// apacheBlock is what one <VirtualHost> block configures.
type apacheBlock struct {
	found        bool
	ssl          bool
	documentRoot string
	certificate  string
	php          *VHostPHP
	// aliases come from ServerAlias: apachectl -S lists them only for
	// addresses with several name-based vhosts.
	aliases []string
}

type apacheParser struct {
	host       *Host
	serverRoot string
	files      map[string][]string
}

func (parser *apacheParser) lines(name string) []string {
	if cached, ok := parser.files[name]; ok {
		return cached
	}
	var result []string
	if info, err := fs.Stat(parser.host.FS, relative(name)); err == nil && info.Size() <= maxApacheConfigFile {
		if data, err := parser.host.readFile(name); err == nil {
			result = joinContinuations(lines(data))
		}
	}
	parser.files[name] = result
	return result
}

// joinContinuations joins lines ending in a backslash with the next line.
// Each returned line keeps the position of its first physical line, so the
// line numbers of apachectl -S still index the result: continued lines are
// replaced by empty ones.
func joinContinuations(physical []string) []string {
	result := make([]string, len(physical))
	for index := 0; index < len(physical); index++ {
		start := index
		line := physical[index]
		for strings.HasSuffix(line, "\\") && index+1 < len(physical) {
			index++
			line = strings.TrimSuffix(line, "\\") + " " + strings.TrimSpace(physical[index])
		}
		result[start] = line
	}
	return result
}

// block parses the <VirtualHost> block that starts at line (1-based).
func (parser *apacheParser) block(name string, line int) apacheBlock {
	content := parser.lines(name)
	if line < 1 || line > len(content) || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(content[line-1])), "<virtualhost") {
		return apacheBlock{}
	}
	block := apacheBlock{found: true}
	budget := apacheIncludeFiles
	parser.directives(content[line:], &block, 0, &budget, true)
	return block
}

// directives applies the directives in content to block. In a virtual host
// block (inBlock) it stops at </VirtualHost>.
func (parser *apacheParser) directives(content []string, block *apacheBlock, depth int, budget *int, inBlock bool) {
	for _, raw := range content {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := apacheFields(line)
		if len(fields) == 0 {
			continue
		}
		directive := strings.ToLower(fields[0])
		argument := ""
		if len(fields) > 1 {
			argument = fields[1]
		}
		switch {
		case directive == "</virtualhost>":
			if inBlock {
				return
			}
		case directive == "documentroot" && block.documentRoot == "":
			block.documentRoot = strings.TrimSuffix(argument, "/")
			if block.documentRoot == "" {
				block.documentRoot = argument
			}
		case directive == "serveralias":
			for _, alias := range fields[1:] {
				block.aliases = append(block.aliases, lowerHostname(alias))
			}
		case directive == "sslengine":
			block.ssl = strings.EqualFold(argument, "on")
		case directive == "sslcertificatefile" && block.certificate == "":
			block.certificate = argument
		case directive == "include" || directive == "includeoptional":
			if depth >= apacheIncludeDepth || argument == "" {
				continue
			}
			for _, included := range parser.glob(absoluteFrom(parser.serverRoot, argument)) {
				if *budget <= 0 {
					return
				}
				*budget--
				parser.directives(parser.lines(included), block, depth+1, budget, false)
			}
		default:
			if php := apachePHPHint(directive, fields[1:]); php != nil && (block.php == nil || phpRank(php) > phpRank(block.php)) {
				block.php = php
			}
		}
	}
}

// apachePHPHint recognises how a directive runs PHP:
//
//	SetHandler "proxy:unix:/run/php/php8.2-fpm.sock|fcgi://localhost"
//	ProxyPassMatch ^/(.*\.php)$ unix:/run/php/php8.1-fpm.sock|fcgi://localhost/var/www/
//	FcgidWrapper /usr/local/php82/bin/php-cgi .php
//	php_admin_value open_basedir /var/www/example
func apachePHPHint(directive string, arguments []string) *VHostPHP {
	joined := strings.Join(arguments, " ")
	lower := strings.ToLower(joined)
	switch directive {
	case "sethandler", "proxypassmatch", "proxypass", "addhandler":
		switch {
		case strings.Contains(lower, "fcgi://") || strings.Contains(lower, "proxy:unix:") && strings.Contains(lower, "php"):
			return newPHP(phpVersionFrom(joined), PHPHandlerFPM)
		case strings.Contains(lower, "x-httpd-lsphp"):
			return newPHP(phpVersionFrom(joined), PHPHandlerLSAPI)
		case directive == "addhandler" && strings.Contains(lower, "fcgid-script"):
			return newPHP(nil, PHPHandlerFastCGI)
		case directive != "proxypass" && strings.Contains(lower, "application/x-httpd-php"):
			return newPHP(phpVersionFrom(joined), PHPHandlerModule)
		}
	case "fcgidwrapper":
		if strings.Contains(lower, "php") {
			return newPHP(phpVersionFrom(joined), PHPHandlerFastCGI)
		}
	case "php_admin_value", "php_admin_flag", "php_value", "php_flag":
		return newPHP(nil, PHPHandlerModule)
	}
	if strings.HasPrefix(directive, "lsapi_") {
		return newPHP(nil, PHPHandlerLSAPI)
	}
	return nil
}

// phpRank prefers hints that name a version, then explicit handlers over
// the php_value fallback.
func phpRank(php *VHostPHP) int {
	rank := 0
	if php.Version != nil {
		rank += 2
	}
	if php.Handler != nil && *php.Handler != PHPHandlerModule {
		rank++
	}
	return rank
}

// globalPHP is the PHP setup that applies to every virtual host without
// its own handler: Debian's conf-enabled/php8.2-fpm.conf or
// mods-enabled/php8.2.load, and EL's conf.d/php.conf or a loaded libphp
// module. FPM wins because its SetHandler overrides mod_php.
func (parser *apacheParser) globalPHP() *VHostPHP {
	root := parser.serverRoot
	for _, pattern := range []string{root + "/conf-enabled/php*-fpm.conf", root + "/conf.d/php.conf", root + "/conf.d/php*-fpm.conf"} {
		for _, name := range parser.glob(pattern) {
			block := apacheBlock{}
			budget := 0
			parser.directives(parser.lines(name), &block, apacheIncludeDepth, &budget, false)
			if block.php != nil && block.php.Handler != nil && *block.php.Handler == PHPHandlerFPM {
				if block.php.Version == nil {
					block.php.Version = phpVersionFrom(path.Base(name))
				}
				return block.php
			}
		}
	}
	if parser.moduleLoaded() {
		return newPHP(parser.moduleVersion(), PHPHandlerModule)
	}
	return nil
}

// moduleLoaded reports whether mod_php is enabled: Debian's
// mods-enabled/php8.2.load, or an uncommented LoadModule of libphp in EL's
// conf.modules.d.
func (parser *apacheParser) moduleLoaded() bool {
	return len(parser.glob(parser.serverRoot+"/mods-enabled/php*.load")) > 0 || parser.moduleLibrary() != ""
}

// moduleVersion returns the version of the enabled mod_php, or nil.
func (parser *apacheParser) moduleVersion() *string {
	for _, name := range parser.glob(parser.serverRoot + "/mods-enabled/php*.load") {
		if version := phpVersionFrom(path.Base(name)); version != nil {
			return version
		}
	}
	return phpVersionFrom(parser.moduleLibrary())
}

func (parser *apacheParser) moduleLibrary() string {
	for _, name := range parser.glob(parser.serverRoot + "/conf.modules.d/*php*.conf") {
		for _, line := range parser.lines(name) {
			fields := apacheFields(strings.TrimSpace(line))
			if len(fields) >= 3 && strings.EqualFold(fields[0], "LoadModule") && strings.Contains(fields[2], "libphp") {
				return fields[2]
			}
		}
	}
	return ""
}

// glob expands an Include pattern. A pattern naming a directory includes
// every file in it, like Apache.
func (parser *apacheParser) glob(pattern string) []string {
	if parser.host.isDir(pattern) {
		pattern = strings.TrimSuffix(pattern, "/") + "/*"
	}
	matches, err := fs.Glob(parser.host.FS, relative(pattern))
	if err != nil {
		return nil
	}
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		if !parser.host.isDir(match) {
			result = append(result, "/"+match)
		}
	}
	slices.Sort(result)
	return result
}

// apacheFields splits a directive line into words, honouring double and
// single quotes.
func apacheFields(line string) []string {
	var fields []string
	var current strings.Builder
	quote := byte(0)
	inField := false
	for index := 0; index < len(line); index++ {
		character := line[index]
		switch {
		case quote != 0:
			if character == '\\' && index+1 < len(line) && line[index+1] == quote {
				index++
				current.WriteByte(quote)
			} else if character == quote {
				quote = 0
			} else {
				current.WriteByte(character)
			}
		case character == '"' || character == '\'':
			quote = character
			inField = true
		case character == ' ' || character == '\t':
			if inField {
				fields = append(fields, current.String())
				current.Reset()
				inField = false
			}
		default:
			current.WriteByte(character)
			inField = true
		}
	}
	if inField {
		fields = append(fields, current.String())
	}
	return fields
}
