package collector

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

const (
	// nginxIncludeDepth bounds nested include expansion.
	nginxIncludeDepth = 8
	// nginxMaxFiles bounds the configuration files read per collection.
	nginxMaxFiles = 5000
	// nginxMaxDirectives bounds the expanded configuration.
	nginxMaxDirectives = 500000
	// maxNginxConfigFile bounds the size of one parsed configuration file.
	maxNginxConfigFile = 4 << 20
)

// nginxConfigPaths are the usual locations of the main configuration file,
// tried after the --conf-path that nginx -V reports.
var nginxConfigPaths = []string{
	"/etc/nginx/nginx.conf",
	"/usr/local/nginx/conf/nginx.conf",
	"/usr/local/etc/nginx/nginx.conf",
	"/opt/nginx/conf/nginx.conf",
	"/usr/local/openresty/nginx/conf/nginx.conf",
}

// nginxDirective is one parsed directive; Block is nil for simple
// directives and non-nil (possibly empty) for blocks.
type nginxDirective struct {
	Name  string
	Args  []string
	File  string
	Line  int
	Block []nginxDirective
}

type nginxToken struct {
	text   string
	line   int
	quoted bool
}

// tokenizeNginx splits a configuration file into words and the special
// tokens ";", "{" and "}". Comments run from an unquoted "#" at the start
// of a word to the end of the line.
func tokenizeNginx(content string) []nginxToken {
	var tokens []nginxToken
	line := 1
	for index := 0; index < len(content); {
		character := content[index]
		switch {
		case character == '\n':
			line++
			index++
		case character == ' ' || character == '\t' || character == '\r':
			index++
		case character == '#':
			for index < len(content) && content[index] != '\n' {
				index++
			}
		case character == ';' || character == '{' || character == '}':
			tokens = append(tokens, nginxToken{text: string(character), line: line})
			index++
		case character == '"' || character == '\'':
			start := line
			var word strings.Builder
			index++
			for index < len(content) && content[index] != character {
				// Like nginx: \" \' and \\ lose the backslash, \t \r \n
				// are control characters, and other escapes (\. in a
				// regular expression) are kept.
				if content[index] == '\\' && index+1 < len(content) {
					switch next := content[index+1]; next {
					case '"', '\'', '\\':
						index++
					case 't', 'r', 'n':
						word.WriteByte(map[byte]byte{'t': '\t', 'r': '\r', 'n': '\n'}[next])
						index += 2
						continue
					default:
						word.WriteByte('\\')
						index++
					}
				}
				if content[index] == '\n' {
					line++
				}
				word.WriteByte(content[index])
				index++
			}
			index++
			tokens = append(tokens, nginxToken{text: word.String(), line: start, quoted: true})
		default:
			start := index
			for index < len(content) {
				c := content[index]
				if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ';' || c == '{' || c == '}' {
					break
				}
				// "${var}" stays one word.
				if c == '$' && index+1 < len(content) && content[index+1] == '{' {
					if end := strings.IndexByte(content[index:], '}'); end > 0 {
						index += end + 1
						continue
					}
				}
				index++
			}
			tokens = append(tokens, nginxToken{text: content[start:index], line: line})
		}
	}
	return tokens
}

// parseNginxTokens builds the directive tree of one file.
func parseNginxTokens(tokens []nginxToken, file string) []nginxDirective {
	position := 0
	var parse func() []nginxDirective
	parse = func() []nginxDirective {
		directives := []nginxDirective{}
		for position < len(tokens) {
			token := tokens[position]
			if !token.quoted && token.text == "}" {
				position++
				return directives
			}
			if !token.quoted && (token.text == ";" || token.text == "{") {
				position++
				continue
			}
			directive := nginxDirective{Name: token.text, File: file, Line: token.line}
			position++
			for position < len(tokens) {
				next := tokens[position]
				if !next.quoted && next.text == ";" {
					position++
					break
				}
				if !next.quoted && next.text == "{" {
					position++
					directive.Block = parse()
					break
				}
				if !next.quoted && next.text == "}" {
					break
				}
				directive.Args = append(directive.Args, next.text)
				position++
			}
			directives = append(directives, directive)
		}
		return directives
	}
	return parse()
}

// nginxConfig is the main configuration file with its includes expanded in
// place.
type nginxConfig struct {
	// prefix resolves relative root paths (nginx's --prefix); confPrefix
	// resolves relative include and certificate paths (the directory of
	// the main configuration file).
	prefix     string
	confPrefix string
	root       []nginxDirective
}

// nginxBuild is what nginx -V reports about the binary's default paths.
type nginxBuild struct {
	prefix   string
	confPath string
}

// nginxBuildInfo runs nginx -V, which only prints the version and the
// configure arguments; unlike nginx -t or -T it does not test the
// configuration or open any log file. Failures leave the zero value.
func (host *Host) nginxBuildInfo(ctx context.Context, binary string) nginxBuild {
	result, err := host.run(ctx, binary, "-V")
	if err != nil || result.ExitCode != 0 {
		return nginxBuild{}
	}
	return parseNginxBuild(append(append([]byte{}, result.Stdout...), result.Stderr...))
}

func parseNginxBuild(output []byte) nginxBuild {
	var build nginxBuild
	for _, line := range lines(output) {
		arguments, ok := strings.CutPrefix(strings.TrimSpace(line), "configure arguments:")
		if !ok {
			continue
		}
		// Quoted arguments such as --with-cc-opt='…' stay one field.
		for _, argument := range apacheFields(strings.TrimSpace(arguments)) {
			if value, ok := strings.CutPrefix(argument, "--prefix="); ok {
				build.prefix = strings.TrimSuffix(value, "/")
			}
			if value, ok := strings.CutPrefix(argument, "--conf-path="); ok {
				build.confPath = value
			}
		}
	}
	if build.confPath != "" && build.prefix != "" {
		build.confPath = absoluteFrom(build.prefix, build.confPath)
	}
	if build.confPath == "" && build.prefix != "" {
		build.confPath = build.prefix + "/conf/nginx.conf"
	}
	if !path.IsAbs(build.confPath) {
		build.confPath = ""
	}
	return build
}

// nginxLoader reads configuration files from disk and expands their
// include directives, bounded in depth, file count and directives.
type nginxLoader struct {
	host       *Host
	confPrefix string
	parsed     map[string][]nginxDirective
	files      int
	directives int
	// missing counts include directives naming a file that does not
	// exist; nginx itself would refuse to start, but one broken site must
	// not hide the others.
	missing int
	// truncated is set when a bound was hit.
	truncated bool
}

func newNginxLoader(host *Host, confPrefix string) *nginxLoader {
	return &nginxLoader{host: host, confPrefix: confPrefix, parsed: make(map[string][]nginxDirective)}
}

// file parses one configuration file, once per collection.
func (loader *nginxLoader) file(name string) ([]nginxDirective, error) {
	if directives, ok := loader.parsed[name]; ok {
		return directives, nil
	}
	if loader.files >= nginxMaxFiles {
		loader.truncated = true
		return nil, fmt.Errorf("more than %d configuration files", nginxMaxFiles)
	}
	info, err := fs.Stat(loader.host.FS, relative(name))
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", name)
	}
	if info.Size() > maxNginxConfigFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, maxNginxConfigFile)
	}
	loader.files++
	data, err := loader.host.readFile(name)
	if err != nil {
		return nil, err
	}
	directives := parseNginxTokens(tokenizeNginx(string(data)), name)
	loader.parsed[name] = directives
	return directives, nil
}

// glob expands an include argument like nginx: a pattern with wildcards
// matches files (not hidden ones, unless the pattern's last element starts
// with a dot), in sorted order, and may match nothing; a plain name must
// exist, and is counted as missing otherwise.
func (loader *nginxLoader) glob(argument string) []string {
	pattern := "/" + relative(absoluteFrom(loader.confPrefix, argument))
	if !strings.ContainsAny(pattern, "*?[") {
		if info, err := fs.Stat(loader.host.FS, relative(pattern)); err != nil || info.IsDir() {
			loader.missing++
			return nil
		}
		return []string{pattern}
	}
	matches, err := fs.Glob(loader.host.FS, relative(pattern))
	if err != nil {
		return nil
	}
	hidden := strings.HasPrefix(path.Base(pattern), ".")
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		if !hidden && strings.HasPrefix(path.Base(match), ".") {
			continue
		}
		if info, err := fs.Stat(loader.host.FS, match); err == nil && !info.IsDir() {
			result = append(result, "/"+match)
		}
	}
	slices.Sort(result)
	return result
}

// expand replaces every include directive with the directives of the files
// it names. Include cycles are cut.
func (loader *nginxLoader) expand(directives []nginxDirective, depth int, stack []string) []nginxDirective {
	result := make([]nginxDirective, 0, len(directives))
	for _, directive := range directives {
		if loader.directives++; loader.directives > nginxMaxDirectives {
			loader.truncated = true
			return result
		}
		if directive.Name == "include" && directive.Block == nil && len(directive.Args) == 1 {
			if depth >= nginxIncludeDepth {
				loader.truncated = true
				continue
			}
			for _, name := range loader.glob(directive.Args[0]) {
				if slices.Contains(stack, name) {
					continue
				}
				included, err := loader.file(name)
				if err != nil {
					continue
				}
				result = append(result, loader.expand(included, depth+1, append(slices.Clip(stack), name))...)
			}
			continue
		}
		if directive.Block != nil {
			directive.Block = loader.expand(directive.Block, depth, stack)
		}
		result = append(result, directive)
	}
	return result
}

// loadNginx reads the main configuration file and everything it includes.
func loadNginx(host *Host, main, prefix string) (nginxConfig, *nginxLoader, error) {
	confPrefix := path.Dir(main)
	loader := newNginxLoader(host, confPrefix)
	directives, err := loader.file(main)
	if err != nil {
		return nginxConfig{}, loader, err
	}
	if prefix == "" {
		prefix = confPrefix
	}
	config := nginxConfig{prefix: prefix, confPrefix: confPrefix, root: loader.expand(directives, 0, []string{main})}
	return config, loader, nil
}

// collectNginx parses the configuration files themselves. It never runs
// nginx -t or -T: their configuration test opens every log file, which
// fails with "Too many open files" on servers with hundreds of sites,
// creates missing log files, and prints nothing when any include is
// missing.
func collectNginx(ctx context.Context, run *vhostRun) ([]VHost, error) {
	host := run.host
	build := host.nginxBuildInfo(ctx, host.findCommand("nginx"))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	candidates := nginxConfigPaths
	if build.confPath != "" {
		candidates = append([]string{build.confPath}, nginxConfigPaths...)
	}
	main := ""
	for _, candidate := range candidates {
		if info, err := fs.Stat(host.FS, relative(candidate)); err == nil && !info.IsDir() {
			main = candidate
			break
		}
	}
	if main == "" {
		return nil, fmt.Errorf("no nginx configuration file found (tried %s)", strings.Join(slices.Compact(candidates), ", "))
	}
	config, loader, err := loadNginx(host, main, build.prefix)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", main, err)
	}
	if loader.missing > 0 {
		run.warn(VHostSourceNginx, fmt.Sprintf("skipped %d include(s) of missing files", loader.missing))
	}
	if loader.truncated {
		run.warn(VHostSourceNginx, fmt.Sprintf("configuration truncated at %d files, %d include levels or %d directives", nginxMaxFiles, nginxIncludeDepth, nginxMaxDirectives))
	}
	return nginxVHosts(config, run), nil
}

// nginxVHosts turns the server blocks of every http block into items.
// Servers whose only names are "_" or "" (catch-all defaults) are skipped.
func nginxVHosts(config nginxConfig, run *vhostRun) []VHost {
	var items []VHost
	for _, http := range config.root {
		if http.Name != "http" || http.Block == nil {
			continue
		}
		inherited := nginxServerDefaults{
			root:        nginxValue(http.Block, "root"),
			certificate: nginxValue(http.Block, "ssl_certificate"),
			upstreams:   make(map[string]string),
		}
		for _, directive := range http.Block {
			if directive.Name == "upstream" && directive.Block != nil && len(directive.Args) == 1 {
				if server := nginxValue(directive.Block, "server"); server != "" {
					inherited.upstreams[directive.Args[0]] = server
				}
			}
		}
		for _, server := range http.Block {
			if server.Name != "server" || server.Block == nil {
				continue
			}
			if item, ok := nginxServer(server, inherited, config, run); ok {
				items = append(items, item)
			}
		}
	}
	return items
}

type nginxServerDefaults struct {
	root        string
	certificate string
	upstreams   map[string]string
}

func nginxServer(server nginxDirective, defaults nginxServerDefaults, config nginxConfig, run *vhostRun) (VHost, bool) {
	var names []string
	for _, directive := range server.Block {
		if directive.Name == "server_name" {
			for _, name := range directive.Args {
				if name != "_" && name != "" {
					names = append(names, lowerHostname(name))
				}
			}
		}
	}
	if len(names) == 0 {
		return VHost{}, false
	}
	// Prefer a plain name over a regular expression as the primary name.
	primary := slices.IndexFunc(names, func(name string) bool { return !strings.HasPrefix(name, "~") })
	if primary < 0 {
		primary = 0
	}
	item := VHost{
		Name:       names[primary],
		Aliases:    slices.Delete(slices.Clone(names), primary, primary+1),
		ConfigFile: optionalString(server.File),
	}

	listens := false
	for _, directive := range server.Block {
		switch directive.Name {
		case "listen":
			if len(directive.Args) == 0 {
				continue
			}
			port, ok := nginxListenPort(directive.Args[0])
			if !ok {
				continue
			}
			listens = true
			item.Ports = append(item.Ports, port)
			if slices.Contains(directive.Args[1:], "ssl") || slices.Contains(directive.Args[1:], "quic") {
				item.SSL = true
			}
		case "ssl":
			if len(directive.Args) == 1 && directive.Args[0] == "on" {
				item.SSL = true
			}
		}
	}
	if !listens {
		item.Ports = []int{80}
	}

	root := nginxValue(server.Block, "root")
	if root == "" {
		for _, location := range server.Block {
			if location.Name == "location" && location.Block != nil && slices.Equal(location.Args, []string{"/"}) {
				root = nginxValue(location.Block, "root")
			}
		}
	}
	if root == "" {
		root = defaults.root
	}
	if root != "" {
		root = absoluteFrom(config.prefix, root)
		if trimmed := strings.TrimSuffix(root, "/"); trimmed != "" {
			root = trimmed
		}
		item.DocumentRoot = &root
	}

	if item.SSL {
		certificate := nginxValue(server.Block, "ssl_certificate")
		if certificate == "" {
			certificate = defaults.certificate
		}
		if certificate != "" && !strings.HasPrefix(certificate, "data:") {
			item.Certificate = run.certificate(absoluteFrom(config.confPrefix, certificate))
		}
	}

	if pass := nginxFind(server.Block, "fastcgi_pass"); pass != "" {
		if upstream, ok := defaults.upstreams[pass]; ok {
			pass = upstream
		}
		handler := PHPHandlerFPM
		if socket, isSocket := strings.CutPrefix(pass, "unix:"); isSocket && !strings.Contains(socket, "fpm") && strings.Contains(socket, "cgi") {
			handler = PHPHandlerFastCGI
		}
		item.PHP = newPHP(phpVersionFrom(pass), handler)
	}
	return item, true
}

// nginxListenPort returns the port of a listen address: "80", "[::]:443",
// "127.0.0.1:8080" or "*:80". A bare host listens on 80; unix sockets
// have no port.
func nginxListenPort(address string) (int, bool) {
	if strings.HasPrefix(address, "unix:") {
		return 0, false
	}
	portText := address
	if index := strings.LastIndexByte(address, ':'); index >= 0 && !strings.HasSuffix(address, "]") {
		portText = address[index+1:]
	} else if strings.HasPrefix(address, "[") || strings.ContainsAny(address, ".") || address == "localhost" || address == "*" {
		return 80, true
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

// nginxValue returns the first argument of the last directive named name
// at this level, which is the one nginx uses for single-valued directives.
func nginxValue(block []nginxDirective, name string) string {
	value := ""
	for _, directive := range block {
		if directive.Name == name && len(directive.Args) > 0 {
			value = directive.Args[0]
		}
	}
	return value
}

// nginxFind returns the first argument of the first directive named name
// at any depth, preferring a unix socket.
func nginxFind(block []nginxDirective, name string) string {
	found := ""
	var walk func([]nginxDirective) bool
	walk = func(directives []nginxDirective) bool {
		for _, directive := range directives {
			if directive.Name == name && len(directive.Args) > 0 {
				if strings.HasPrefix(directive.Args[0], "unix:") {
					found = directive.Args[0]
					return true
				}
				if found == "" {
					found = directive.Args[0]
				}
			}
			if directive.Block != nil && walk(directive.Block) {
				return true
			}
		}
		return false
	}
	walk(block)
	return found
}
